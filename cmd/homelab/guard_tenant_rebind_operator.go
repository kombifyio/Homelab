package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/routes"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/db"
	"github.com/kombifyio/homelab/pkg/db/tenantcontext"
	"github.com/kombifyio/homelab/pkg/guardidentity"
	"github.com/kombifyio/homelab/pkg/workerauth"
)

const guardTenantRebindUsage = "usage: homelab guard-tenant-rebind stage|commit|status --from <legacy tenant> --to usr:<sub> --subject <sub> [--legacy-label <label>] [--worker <id> ...] [--managed-lease <lease id> ...] [--apply]\n" +
	"  stage   (legacy tenant, before the row move) offers every connected Guard of the subject a rebind to usr:<sub>\n" +
	"  commit  (canonical tenant, after the row move, before Homelab Runtime serves again) pins the offered credential\n" +
	"  status  lists the directives under both tenants\n" +
	"  Without --apply nothing is written. --legacy-label is required when --from is not the bare subject.\n" +
	"  --managed-lease also offers the Guard of that managed runtime server; its provider custody moves with\n" +
	"  `homelab managed-runtime-retenant`, and commit pins it only once the lease is readmitted under usr:<sub>."

// guardTenantRebindStore is the part of the control-plane store the command
// uses; guardTenantRebindPostgresStore satisfies it.
type guardTenantRebindStore interface {
	ListWorkersByTenant(ctx context.Context, tenantID string) ([]controlplane.Worker, error)
	controlplane.WorkerTenantRebindStore
	controlplane.WorkerCredentialStore
	// ManagedLeaseCustodyHeldBy reports whether tenantID holds the native
	// provider custody (execution authority) of the live lease leaseID, i.e.
	// the lease was readmitted there, not only moved by the setup re-key.
	ManagedLeaseCustodyHeldBy(ctx context.Context, tenantID, leaseID string) (bool, error)
}

type guardTenantRebindArgs struct {
	action, from, to, subject, label string
	workers                          map[string]bool
	managedLeases                    map[string]bool
	apply                            bool
	lifetime                         time.Duration
}

// guardTenantRebindPostgresStore adds the managed lease readback to the
// control-plane store.
type guardTenantRebindPostgresStore struct {
	*controlplane.PostgresStore
	database *sql.DB
}

func (s guardTenantRebindPostgresStore) ManagedLeaseCustodyHeldBy(ctx context.Context, tenantID, leaseID string) (bool, error) {
	tx, err := s.database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tenantcontext.ApplyTenant(ctx, tx, tenantID); err != nil {
		return false, err
	}
	var held bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM techstack_vm_leases AS lease
			JOIN runtime_lease_execution_authorities AS authority
			  ON authority.tenant_id = lease.tenant_id AND authority.lease_id = lease.id
			WHERE lease.tenant_id = $1 AND lease.id = $2 AND lease.cancelled_at IS NULL
			  AND lease.desired_state IN ('running', 'stopped')
		)
	`, tenantID, leaseID).Scan(&held)
	return held, err
}

// workerManagedLease names the managed runtime lease a Guard runs under, if any.
func workerManagedLease(worker controlplane.Worker) string {
	for _, key := range []string{"lease_id", "runtime_lease_id"} {
		if value, ok := worker.Capabilities[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// runGuardTenantRebind is the operator side of the same-subject Guard tenant
// rebind (platform-ilxzn). It never runs against a host: the Guard takes the
// offer on its own authenticated heartbeat.
func runGuardTenantRebind(ctx context.Context, args []string) error {
	parsed, err := parseGuardTenantRebindArgs(args, os.Stderr)
	if err != nil {
		return err
	}
	store, release, err := openGuardTenantRebindStore(ctx)
	if err != nil {
		return err
	}
	defer release()
	return runGuardTenantRebindWith(ctx, parsed, store, workerauth.SecretFromEnv(), time.Now().UTC(), os.Stdout)
}

func parseGuardTenantRebindArgs(args []string, stderr io.Writer) (guardTenantRebindArgs, error) {
	var out guardTenantRebindArgs
	if len(args) == 0 {
		return out, errors.New(guardTenantRebindUsage)
	}
	out.action = args[0]
	flags := flag.NewFlagSet("guard-tenant-rebind "+out.action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	from := flags.String("from", "", "legacy tenant (bare subject or the mapped label)")
	to := flags.String("to", "", "canonical tenant usr:<sub>")
	subject := flags.String("subject", "", "auth subject of the person")
	label := flags.String("legacy-label", "", "legacy tenant label the operator maps to --subject")
	apply := flags.Bool("apply", false, "write the change (default: dry run)")
	lifetime := flags.Duration("lifetime", guardidentity.TenantRebindDefaultLifetime, "stage: how long the offer stays valid")
	var workers stringListFlag
	flags.Var(&workers, "worker", "restrict to these worker ids (repeat)")
	var managedLeases stringListFlag
	flags.Var(&managedLeases, "managed-lease", "also offer the Guard of this managed runtime lease, moved by managed-runtime-retenant (repeat)")
	if err := flags.Parse(args[1:]); err != nil {
		return out, fmt.Errorf("%w\n%s", err, guardTenantRebindUsage)
	}
	if flags.NArg() != 0 {
		return out, errors.New(guardTenantRebindUsage)
	}
	switch out.action {
	case "stage", "commit", "status":
	default:
		return out, errors.New(guardTenantRebindUsage)
	}
	out.from, out.to, out.subject, out.label = strings.TrimSpace(*from), strings.TrimSpace(*to), strings.TrimSpace(*subject), strings.TrimSpace(*label)
	if err := guardidentity.ValidateTenantRebindPair(out.from, out.to, out.subject, out.label); err != nil {
		return out, err
	}
	if *lifetime <= time.Minute || *lifetime > guardidentity.TenantRebindMaxLifetime {
		return out, fmt.Errorf("--lifetime must be between 1m and %s", guardidentity.TenantRebindMaxLifetime)
	}
	out.apply, out.lifetime = *apply, *lifetime
	out.workers = map[string]bool{}
	for _, id := range workers {
		if id = strings.TrimSpace(id); id != "" {
			out.workers[id] = true
		}
	}
	out.managedLeases = map[string]bool{}
	for _, id := range managedLeases {
		if id = strings.TrimSpace(id); id != "" {
			out.managedLeases[id] = true
		}
	}
	return out, nil
}

type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }
func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func runGuardTenantRebindWith(ctx context.Context, args guardTenantRebindArgs, store guardTenantRebindStore, secret []byte, now time.Time, stdout io.Writer) error {
	if len(secret) == 0 {
		return errors.New("the worker credential secret is not configured in this environment")
	}
	switch args.action {
	case "stage":
		return stageGuardTenantRebind(ctx, args, store, secret, now, stdout)
	case "commit":
		return commitGuardTenantRebind(ctx, args, store, secret, now, stdout)
	default:
		return statusGuardTenantRebind(ctx, args, store, stdout)
	}
}

func mode(apply bool) string {
	if apply {
		return "apply"
	}
	return "dry-run"
}

// stageGuardTenantRebind offers the rebind to every worker of the legacy
// tenant. The label mapping is accepted only when every worker of the tenant
// belongs to the subject: a tenant shared with anyone else is not the legacy
// form of one person.
func stageGuardTenantRebind(ctx context.Context, args guardTenantRebindArgs, store guardTenantRebindStore, secret []byte, now time.Time, stdout io.Writer) error {
	workers, err := store.ListWorkersByTenant(ctx, args.from)
	if err != nil {
		return fmt.Errorf("list the legacy tenant's workers: %w", err)
	}
	for _, worker := range workers {
		if owner := strings.TrimSpace(worker.OwnerSubjectID); owner != "" && owner != args.subject {
			return fmt.Errorf("%w: worker %s of %s belongs to another subject; the tenant is not the legacy form of %s",
				guardidentity.ErrTenantRebindRefused, worker.ID, args.from, args.subject)
		}
	}
	sortWorkers(workers)
	staged, skipped := 0, 0
	for _, worker := range workers {
		if len(args.workers) > 0 && !args.workers[worker.ID] {
			continue
		}
		reason, directive := planGuardTenantRebind(args, worker, now)
		if reason != "" {
			skipped++
			_, _ = fmt.Fprintf(stdout, "GUARD_TENANT_REBIND_SKIP worker=%s status=%s reason=%s\n", worker.ID, worker.Status, reason)
			continue
		}
		// The credential must be derivable now; the heartbeat derives the same.
		if _, prepErr := routes.PrepareGuardTenantRebindCredential(secret, directive, worker); prepErr != nil {
			return fmt.Errorf("worker %s: %w", worker.ID, prepErr)
		}
		replaceID := ""
		if existing, ok := guardidentity.DirectiveFromResource(worker.Resources[controlplane.WorkerTenantRebindResourceKey]); ok && existing.DirectiveID != directive.DirectiveID {
			replaceID = existing.DirectiveID
		}
		if args.apply {
			value, encodeErr := directive.ResourceValue()
			if encodeErr != nil {
				return encodeErr
			}
			if stageErr := store.StageWorkerTenantRebind(ctx, args.from, worker.ID, directive.DirectiveID, replaceID, value); stageErr != nil {
				return fmt.Errorf("stage worker %s: %w", worker.ID, stageErr)
			}
		}
		staged++
		_, _ = fmt.Fprintf(stdout, "GUARD_TENANT_REBIND_STAGE mode=%s worker=%s server=%s directive=%s from=%s to=%s not_after=%s\n",
			mode(args.apply), worker.ID, directive.ServerID, directive.DirectiveID, directive.FromTenantID,
			directive.ToTenantID, directive.NotAfter.Format(time.RFC3339))
	}
	_, _ = fmt.Fprintf(stdout, "GUARD_TENANT_REBIND_STAGE_DONE mode=%s staged=%d skipped=%d\n", mode(args.apply), staged, skipped)
	return nil
}

func planGuardTenantRebind(args guardTenantRebindArgs, worker controlplane.Worker, now time.Time) (string, guardidentity.TenantRebindDirective) {
	if strings.EqualFold(worker.Status, "rejected") || strings.EqualFold(worker.Status, "revoked") || !worker.Approved {
		return "worker_not_approved", guardidentity.TenantRebindDirective{}
	}
	if strings.TrimSpace(worker.OwnerSubjectID) != args.subject {
		return "worker_has_no_owner_subject", guardidentity.TenantRebindDirective{}
	}
	// A managed runtime server is held by its provider custody under the legacy
	// tenant; the row move leaves it there, so its Guard must not move alone.
	// The operator names the leases whose custody moves with
	// `homelab managed-runtime-retenant`; only their Guards are offered.
	if lease := workerManagedLease(worker); lease != "" && !args.managedLeases[lease] {
		return "managed_runtime_lease_stays_with_provider_custody", guardidentity.TenantRebindDirective{}
	}
	serverID := strings.TrimSpace(fmt.Sprint(worker.Capabilities["server_id"]))
	if serverID == "" || serverID == "<nil>" {
		return "worker_names_no_server", guardidentity.TenantRebindDirective{}
	}
	state, err := controlplane.WorkerCredentialStateFromWorker(worker)
	if err != nil || state.TokenSHA256 == "" {
		return "worker_has_no_dedicated_credential", guardidentity.TenantRebindDirective{}
	}
	if existing, ok := guardidentity.DirectiveFromResource(worker.Resources[controlplane.WorkerTenantRebindResourceKey]); ok {
		if existing.Validate(now) == nil && existing.FromTenantID == args.from && existing.ToTenantID == args.to {
			// Re-staging keeps the offered directive (and so the credential).
			return "", existing
		}
		if existing.Validate(now) == nil {
			return "worker_holds_another_directive", guardidentity.TenantRebindDirective{}
		}
		// An expired directive is replaced by a new one (stageGuardTenantRebind
		// names it as the one to replace).
	}
	id, err := guardidentity.NewTenantRebindDirectiveID()
	if err != nil {
		return "directive_id_unavailable", guardidentity.TenantRebindDirective{}
	}
	directive := guardidentity.TenantRebindDirective{
		ContractVersion: guardidentity.TenantRebindContractVersion, DirectiveID: id,
		FromTenantID: args.from, ToTenantID: args.to, SubjectID: args.subject,
		RuntimeAgentID: worker.ID, ServerID: serverID, StackID: strings.TrimSpace(worker.StackID),
		CredentialGeneration: state.Generation, IssuedAt: now, NotAfter: now.Add(args.lifetime),
	}
	if err := directive.Validate(now); err != nil {
		return "directive_invalid", guardidentity.TenantRebindDirective{}
	}
	return "", directive
}

// commitGuardTenantRebind runs after the rows moved to the canonical tenant
// and before Homelab serves again: for every moved worker whose Guard
// acknowledged the offer it pins the offered credential (one credential
// generation step) and removes the directive. A worker whose Guard never
// acknowledged keeps its directive and is reported: that host needs a new
// pairing.
func commitGuardTenantRebind(ctx context.Context, args guardTenantRebindArgs, store guardTenantRebindStore, secret []byte, now time.Time, stdout io.Writer) error {
	notMoved := 0
	if legacy, err := store.ListWorkersByTenant(ctx, args.from); err == nil {
		sortWorkers(legacy)
		for _, worker := range legacy {
			if directive, ok := guardidentity.DirectiveFromResource(worker.Resources[controlplane.WorkerTenantRebindResourceKey]); ok {
				// Its rows did not move: the Guard keeps working under the
				// legacy tenant and its offer expires unused.
				notMoved++
				_, _ = fmt.Fprintf(stdout, "GUARD_TENANT_REBIND_NOT_MOVED worker=%s directive=%s tenant=%s\n", worker.ID, directive.DirectiveID, args.from)
			}
		}
	}
	workers, err := store.ListWorkersByTenant(ctx, args.to)
	if err != nil {
		return fmt.Errorf("list the canonical tenant's workers: %w", err)
	}
	sortWorkers(workers)
	committed, pending, failed := 0, 0, 0
	for _, worker := range workers {
		if len(args.workers) > 0 && !args.workers[worker.ID] {
			continue
		}
		directive, ok := guardidentity.DirectiveFromResource(worker.Resources[controlplane.WorkerTenantRebindResourceKey])
		if !ok || directive.FromTenantID != args.from || directive.ToTenantID != args.to {
			continue
		}
		// Each worker commits on its own; a failure is reported and the rest
		// continue, and a rerun resumes (an already pinned credential only
		// removes the directive).
		outcome, commitErr := commitOneGuardTenantRebind(ctx, args, store, secret, now, worker, directive)
		switch {
		case commitErr != nil:
			failed++
			_, _ = fmt.Fprintf(stdout, "GUARD_TENANT_REBIND_FAILED worker=%s directive=%s error=%q\n", worker.ID, directive.DirectiveID, commitErr.Error())
		case outcome != "":
			pending++
			_, _ = fmt.Fprintf(stdout, "GUARD_TENANT_REBIND_PENDING worker=%s directive=%s reason=%s\n", worker.ID, directive.DirectiveID, outcome)
		default:
			committed++
			_, _ = fmt.Fprintf(stdout, "GUARD_TENANT_REBIND_COMMIT mode=%s worker=%s directive=%s\n", mode(args.apply), worker.ID, directive.DirectiveID)
		}
	}
	_, _ = fmt.Fprintf(stdout, "GUARD_TENANT_REBIND_COMMIT_DONE mode=%s committed=%d pending=%d failed=%d not_moved=%d\n",
		mode(args.apply), committed, pending, failed, notMoved)
	if failed > 0 {
		return fmt.Errorf("%d worker(s) could not be committed; repeat the commit", failed)
	}
	return nil
}

// commitOneGuardTenantRebind returns a pending reason, or "" once the worker's
// canonical credential is pinned and its directive removed.
func commitOneGuardTenantRebind(ctx context.Context, args guardTenantRebindArgs, store guardTenantRebindStore, secret []byte, now time.Time,
	worker controlplane.Worker, directive guardidentity.TenantRebindDirective) (string, error) {
	if worker.TenantID != args.to {
		return "", fmt.Errorf("%w: the canonical listing returned a row of tenant %s", guardidentity.ErrTenantRebindRefused, worker.TenantID)
	}
	credential, err := routes.PrepareGuardTenantRebindCredential(secret, directive, worker)
	if err != nil {
		return "", err
	}
	if lease := workerManagedLease(worker); lease != "" && !credential.Committed {
		// The Guard of a managed server switches only once its provider
		// custody is readmitted under the canonical tenant.
		held, heldErr := store.ManagedLeaseCustodyHeldBy(ctx, args.to, lease)
		if heldErr != nil {
			return "", fmt.Errorf("read managed lease %s: %w", lease, heldErr)
		}
		if !held {
			return "managed_lease_not_readmitted", nil
		}
	}
	if !credential.Committed {
		switch {
		case directive.StagedAt == nil:
			return "guard_never_acknowledged", nil
		case directive.Validate(now) != nil:
			return "directive_expired", nil
		}
	}
	if !args.apply {
		return "", nil
	}
	if !credential.Committed {
		if _, casErr := store.CompareAndSwapWorkerCredential(ctx, controlplane.WorkerCredentialCAS{
			TenantID: args.to, WorkerID: worker.ID, Expected: credential.Expected, Next: credential.Next,
		}); casErr != nil {
			return "", fmt.Errorf("pin the canonical credential: %w", casErr)
		}
	}
	if clearErr := store.ClearWorkerTenantRebind(ctx, args.to, worker.ID, directive.DirectiveID); clearErr != nil {
		return "", fmt.Errorf("remove the directive: %w", clearErr)
	}
	return "", nil
}

func statusGuardTenantRebind(ctx context.Context, args guardTenantRebindArgs, store guardTenantRebindStore, stdout io.Writer) error {
	for _, tenant := range []string{args.from, args.to} {
		workers, err := store.ListWorkersByTenant(ctx, tenant)
		if err != nil {
			return fmt.Errorf("list workers of %s: %w", tenant, err)
		}
		sortWorkers(workers)
		for _, worker := range workers {
			directive, ok := guardidentity.DirectiveFromResource(worker.Resources[controlplane.WorkerTenantRebindResourceKey])
			if !ok {
				_, _ = fmt.Fprintf(stdout, "GUARD_TENANT_REBIND_STATUS tenant=%s worker=%s directive=none\n", tenant, worker.ID)
				continue
			}
			staged := "no"
			if directive.StagedAt != nil {
				staged = directive.StagedAt.UTC().Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(stdout, "GUARD_TENANT_REBIND_STATUS tenant=%s worker=%s directive=%s from=%s to=%s acknowledged=%s not_after=%s\n",
				tenant, worker.ID, directive.DirectiveID, directive.FromTenantID, directive.ToTenantID, staged,
				directive.NotAfter.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

func sortWorkers(workers []controlplane.Worker) {
	sort.Slice(workers, func(i, j int) bool { return workers[i].ID < workers[j].ID })
}

func openGuardTenantRebindStore(ctx context.Context) (guardTenantRebindStore, func(), error) {
	cfg, err := db.ConfigFromEnv()
	if err != nil {
		return nil, nil, fmt.Errorf("guard tenant rebind requires the installation database configuration: %w", err)
	}
	if cfg.Backend != db.StoreBackendPostgres {
		return nil, nil, fmt.Errorf("guard tenant rebind reads the control-plane Postgres database: set %s", db.EnvDatabaseURL)
	}
	hostedSystem := strings.TrimSpace(os.Getenv(db.EnvRuntimeSystemDatabaseURL)) != ""
	if hostedSystem {
		if cfg, err = db.RuntimeSystemConfig(cfg); err != nil {
			return nil, nil, fmt.Errorf("guard tenant rebind system login: %w", err)
		}
	}
	database, err := db.Open(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("guard tenant rebind could not open the installation database: %w", err)
	}
	release := func() { _ = database.Close() }
	if err := db.VerifyExpectedDatabaseIdentity(ctx, database.DB); err != nil {
		release()
		return nil, nil, fmt.Errorf("guard tenant rebind database identity was not verified: %w", err)
	}
	if hostedSystem {
		if err := db.VerifyRuntimeSystemLogin(ctx, database); err != nil {
			release()
			return nil, nil, fmt.Errorf("guard tenant rebind system login was not verified: %w", err)
		}
	}
	return guardTenantRebindPostgresStore{PostgresStore: controlplane.NewPostgresStore(database.DB), database: database.DB}, release, nil
}
