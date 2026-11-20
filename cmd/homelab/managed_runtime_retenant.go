package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/db"
	"github.com/kombifyio/homelab/pkg/guardidentity"
	"github.com/kombifyio/homelab/pkg/logger"
)

const managedRuntimeRetenantUsage = "usage: homelab managed-runtime-retenant archive|readmit|adopt|status --from <legacy tenant> --to usr:<sub> --subject <sub> --lease <lease id> [--legacy-label <label>] [--apply]\n" +
	"  archive  (before the setup re-key) copies the lease's legacy provider custody verbatim into techstack_archive\n" +
	"           and removes it from the live tables; the custody-free lease row then moves with its server in the re-key\n" +
	"  readmit  (after the setup re-key) re-admits the same lease and resource generation under usr:<sub> and parks its\n" +
	"           provision behind a dispatch guard bound to the archive; no provider call\n" +
	"  adopt    resolves the parked provision with the existing provision resolution (read-only provider discovery by\n" +
	"           the generation's correlation name, then the append-only adopt decision); never creates or deletes\n" +
	"  status   shows where the lease's custody is and how far the adoption went\n" +
	"  Without --apply nothing is committed (archive and readmit roll back; adopt makes no provider call). Every step\n" +
	"  needs the migration owner in DATABASE_URL; readmit and adopt also need KOMBIFY_EDITION and the provider\n" +
	"  credential bundles of the Homelab Runtime service. archive and readmit (also as a dry run) briefly lock the shared\n" +
	"  custody tables: run them in the reset window with Homelab Runtime quiesced."

// managedRuntimeRetenantSubject is the operator identity recorded on the
// adopt decision.
const managedRuntimeRetenantSubject = "operator:managed-runtime-retenant"

type managedRuntimeRetenantArgs struct {
	action string
	req    providercontrol.CustodyRetenantRequest
}

// runManagedRuntimeRetenant is the operator side of moving a live managed
// runtime server from a person's legacy tenant to usr:<sub> without touching
// the provider VM (platform-ilxzn). The Guard on that server moves with
// `homelab guard-tenant-rebind --managed-lease <lease id>`.
func runManagedRuntimeRetenant(ctx context.Context, args []string) error {
	parsed, err := parseManagedRuntimeRetenantArgs(args, os.Stderr)
	if err != nil {
		return err
	}
	database, release, err := openManagedRuntimeRetenantDatabase(ctx)
	if err != nil {
		return err
	}
	defer release()
	switch parsed.action {
	case "archive":
		archive, err := providercontrol.ArchiveManagedRuntimeCustody(ctx, database.DB, parsed.req)
		if err != nil {
			return err
		}
		printManagedRuntimeArchive(os.Stdout, parsed.req, archive)
		return nil
	case "readmit":
		composed, err := composeManagedRuntimeRetenantControl(database)
		if err != nil {
			return err
		}
		readmission, err := composed.admission.ReadmitArchivedManagedRuntime(ctx, parsed.req)
		if err != nil {
			return err
		}
		printManagedRuntimeReadmission(os.Stdout, parsed.req, readmission)
		return nil
	case "adopt":
		return adoptManagedRuntimeRetenant(ctx, database, parsed.req, os.Stdout)
	default:
		status, err := providercontrol.ManagedRuntimeRetenantStatus(ctx, database.DB, parsed.req)
		if err != nil {
			return err
		}
		printManagedRuntimeStatus(os.Stdout, parsed.req, status)
		return nil
	}
}

func composeManagedRuntimeRetenantControl(database *db.DB) (nativeProviderControl, error) {
	edition := config.Edition(strings.TrimSpace(os.Getenv("KOMBIFY_EDITION")))
	if !edition.IsValid() {
		return nativeProviderControl{}, errors.New("managed-runtime-retenant needs KOMBIFY_EDITION of the Homelab Runtime service")
	}
	composed, err := composeProviderControlForRetenant(database.DB, edition, logger.Get())
	if err != nil {
		return nativeProviderControl{}, fmt.Errorf("compose provider control: %w", err)
	}
	if composed.admission == nil || composed.runtime == nil {
		return nativeProviderControl{}, errors.New("provider control composed no native admission")
	}
	return composed, nil
}

// adoptManagedRuntimeRetenant runs the existing provision resolution for the
// readmitted operation. The provider-control worker never lists a guarded
// at-most-once provision, so a provision that is parked from its first
// committed state is resolved here, with the same application the automatic
// resolver and the hosted Operator Reconcile use.
func adoptManagedRuntimeRetenant(ctx context.Context, database *db.DB, req providercontrol.CustodyRetenantRequest, out io.Writer) error {
	status, err := providercontrol.ManagedRuntimeRetenantStatus(ctx, database.DB, req)
	if err != nil {
		return err
	}
	switch {
	case !status.ReadmittedUnderTo:
		return fmt.Errorf("%w: lease %s is not readmitted under %s; run readmit first", providercontrol.ErrCustodyRetenantRefused, req.LeaseID, req.ToTenantID)
	case status.ResolutionOutcome == string(providercontrol.ProvisionResolutionAdoptedExactCandidate):
		printManagedRuntimeStatus(out, req, status)
		return nil
	case status.OperationPhase != "accepted":
		return fmt.Errorf("%w: operation %s is %s/%s, not a parked provision", providercontrol.ErrCustodyRetenantRefused, status.OperationID, status.OperationStatus, status.OperationPhase)
	}
	if !req.Apply {
		_, _ = fmt.Fprintf(out, "MANAGED_RUNTIME_RETENANT_ADOPT mode=dry-run tenant=%s operation=%s lease=%s provider_call=none\n",
			req.ToTenantID, status.OperationID, req.LeaseID)
		return nil
	}
	composed, err := composeManagedRuntimeRetenantControl(database)
	if err != nil {
		return err
	}
	result, err := composed.runtime.ResolveProvisionOperation(ctx, providercontrol.ProvisionResolutionWorkflowRequest{
		TenantID: req.ToTenantID, OperationID: status.OperationID,
		OperatorSubjectID: managedRuntimeRetenantSubject, Confirmation: "adopt-provision:" + status.OperationID,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "MANAGED_RUNTIME_RETENANT_ADOPT mode=apply tenant=%s operation=%s lease=%s outcome=%s candidates=%d phase=%s resources=%d decision=%s\n",
		req.ToTenantID, status.OperationID, req.LeaseID, result.Decision.Outcome, len(result.Observation.Candidates),
		result.Operation.Head.Phase, len(result.Operation.Head.Resources), result.Decision.DecisionDigest)
	if result.Decision.Outcome != providercontrol.ProvisionResolutionAdoptedExactCandidate {
		return fmt.Errorf("the provider resources were not adopted (%s); nothing was created or deleted", result.Decision.Outcome)
	}
	return nil
}

func parseManagedRuntimeRetenantArgs(args []string, stderr io.Writer) (managedRuntimeRetenantArgs, error) {
	var out managedRuntimeRetenantArgs
	if len(args) == 0 {
		return out, errors.New(managedRuntimeRetenantUsage)
	}
	out.action = args[0]
	switch out.action {
	case "archive", "readmit", "adopt", "status":
	default:
		return out, errors.New(managedRuntimeRetenantUsage)
	}
	flags := flag.NewFlagSet("managed-runtime-retenant "+out.action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	from := flags.String("from", "", "legacy tenant (bare subject or the mapped label)")
	to := flags.String("to", "", "canonical tenant usr:<sub>")
	subject := flags.String("subject", "", "auth subject of the person")
	label := flags.String("legacy-label", "", "legacy tenant label the operator maps to --subject")
	lease := flags.String("lease", "", "managed runtime lease id")
	apply := flags.Bool("apply", false, "commit the change (default: dry run)")
	if err := flags.Parse(args[1:]); err != nil {
		return out, fmt.Errorf("%w\n%s", err, managedRuntimeRetenantUsage)
	}
	if flags.NArg() != 0 {
		return out, errors.New(managedRuntimeRetenantUsage)
	}
	out.req = providercontrol.CustodyRetenantRequest{
		FromTenantID: strings.TrimSpace(*from), ToTenantID: strings.TrimSpace(*to),
		SubjectID: strings.TrimSpace(*subject), LeaseID: strings.TrimSpace(*lease), Apply: *apply,
	}
	// The same pair rule as the Guard rebind: the target is usr:<subject> and
	// the source is the bare subject or a label the operator maps to it.
	if err := guardidentity.ValidateTenantRebindPair(out.req.FromTenantID, out.req.ToTenantID, out.req.SubjectID, strings.TrimSpace(*label)); err != nil {
		return out, err
	}
	if out.req.LeaseID == "" {
		return out, fmt.Errorf("--lease is required\n%s", managedRuntimeRetenantUsage)
	}
	return out, nil
}

// openManagedRuntimeRetenantDatabase opens the installation database as given.
// Every step reads the owner-only archive schema and archive, readmit and adopt
// write immutable custody, so the command runs only as the owner of those
// tables (the migration role), never the runtime system login.
func openManagedRuntimeRetenantDatabase(ctx context.Context) (*db.DB, func(), error) {
	cfg, err := db.ConfigFromEnv()
	if err != nil {
		return nil, nil, fmt.Errorf("managed-runtime-retenant requires the installation database configuration: %w", err)
	}
	if cfg.Backend != db.StoreBackendPostgres {
		return nil, nil, fmt.Errorf("managed-runtime-retenant works on the control-plane Postgres database: set %s", db.EnvDatabaseURL)
	}
	database, err := db.Open(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("managed-runtime-retenant could not open the installation database: %w", err)
	}
	release := func() { _ = database.Close() }
	if err := db.VerifyExpectedDatabaseIdentity(ctx, database.DB); err != nil {
		release()
		return nil, nil, fmt.Errorf("managed-runtime-retenant database identity was not verified: %w", err)
	}
	{
		var owner bool
		if err := database.DB.QueryRowContext(ctx, `
			SELECT pg_has_role(current_user, tableowner, 'USAGE')
			FROM pg_tables WHERE schemaname = 'public' AND tablename = 'provider_operations'
		`).Scan(&owner); err != nil || !owner {
			release()
			return nil, nil, errors.New("managed-runtime-retenant must run as the owner of the provider custody tables (the migration role)")
		}
	}
	return database, release, nil
}

func printManagedRuntimeArchive(out io.Writer, req providercontrol.CustodyRetenantRequest, archive providercontrol.CustodyRetenantArchive) {
	state := "archived"
	if archive.AlreadyArchived {
		state = "already_archived"
	}
	tables := make([]string, 0, len(archive.Rows))
	for table, count := range archive.Rows {
		if count > 0 {
			tables = append(tables, fmt.Sprintf("%s=%d", table, count))
		}
	}
	sort.Strings(tables)
	_, _ = fmt.Fprintf(out, "MANAGED_RUNTIME_RETENANT_ARCHIVE mode=%s state=%s retenant=%s lease=%s generation=%s server=%s stack=%s slot=%s from=%s to=%s manifest=%s rows=%s\n",
		mode(req.Apply), state, archive.RetenantID, archive.LeaseID, archive.ResourceGenerationID,
		archive.ServerID, archive.StackID, archive.SlotKey, req.FromTenantID, req.ToTenantID, archive.ManifestDigest,
		strings.Join(tables, ","))
}

func printManagedRuntimeReadmission(out io.Writer, req providercontrol.CustodyRetenantRequest, readmission providercontrol.CustodyReadmission) {
	if readmission.AlreadyReadmitted {
		_, _ = fmt.Fprintf(out, "MANAGED_RUNTIME_RETENANT_READMIT mode=%s state=already_readmitted retenant=%s lease=%s generation=%s operation=%s to=%s manifest=%s\n",
			mode(req.Apply), readmission.RetenantID, readmission.LeaseID, readmission.ResourceGenerationID,
			readmission.OperationID, req.ToTenantID, readmission.ManifestDigest)
		return
	}
	_, _ = fmt.Fprintf(out, "MANAGED_RUNTIME_RETENANT_READMIT mode=%s state=readmitted_parked retenant=%s lease=%s generation=%s server=%s slot=%s operation=%s to=%s manifest=%s host_key_pins=%d held_capacity=%d capacity_limit=%d\n",
		mode(req.Apply), readmission.RetenantID, readmission.LeaseID,
		readmission.ResourceGenerationID, readmission.ServerID, readmission.RuntimeSlotID, readmission.OperationID,
		req.ToTenantID, readmission.ManifestDigest, readmission.HostKeyPinsRebound, readmission.HeldCapacity,
		readmission.CapacityLimit)
}

func printManagedRuntimeStatus(out io.Writer, req providercontrol.CustodyRetenantRequest, status providercontrol.CustodyRetenantStatus) {
	_, _ = fmt.Fprintf(out, "MANAGED_RUNTIME_RETENANT_STATUS lease=%s from=%s legacy_custody_live=%t archived=%t archive_rows=%d manifest=%s "+
		"to=%s server_under_to=%t server_lease=%s readmitted=%t operation=%s status=%s phase=%s resolution=%s\n",
		req.LeaseID, req.FromTenantID, status.LiveUnderFrom, status.Archived, status.ArchiveRows,
		orNone(status.ManifestDigest), req.ToTenantID, status.ServerUnderTo, orNone(status.ServerLeaseBinding),
		status.ReadmittedUnderTo, orNone(status.OperationID), orNone(status.OperationStatus),
		orNone(status.OperationPhase), orNone(status.ResolutionOutcome))
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}
