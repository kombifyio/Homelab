package providercontrol

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/runtimeproduct/vmlease"
	"github.com/kombifyio/homelab/pkg/vmleases"
	"github.com/kombifyio/stackkits/contracts/providerexecutor/v1beta1"
	"github.com/lib/pq"
)

// A managed runtime lease moves from a person's legacy tenant to usr:<sub>
// (platform-ilxzn) in two operator steps around the setup-row re-key, with the
// provider resources untouched:
//
//  1. ArchiveManagedRuntimeCustody copies the legacy custody graph of the lease
//     verbatim into techstack_archive.custody_retenant_rows and removes it from
//     the live tables, so the lease's server and stack hold no provider custody
//     and move with the re-key like any other setup row.
//  2. NativeAdmission.ReadmitArchivedManagedRuntime admits the same lease id and
//     resource generation under usr:<sub> and parks its provision behind a
//     dispatch guard bound to the archive, in one transaction and without any
//     provider call. The existing automatic provision resolver then finds the
//     provider resources by their generation correlation name and adopts them
//     with fresh evidence.
//
// Nothing here rewrites a hash-bound row: the legacy history stays readable in
// the archive, and the new custody is produced by the normal admission,
// receipt and resolution code under the new tenant.

const (
	custodyRetenantArchiveTable    = "techstack_archive.custody_retenant_rows"
	custodyRetenantDigestVersion   = "providercontrol/managed-runtime-retenant/v1"
	custodyRetenantAdoptionVersion = "providercontrol/managed-runtime-retenant-adoption/v1"
	custodyRetenantAdmissionDigest = "providercontrol/native-provision-readmission/v1"
	custodyRetenantClaimOwner      = "operator:managed-runtime-retenant"
	custodyRetenantPersonPrefix    = "usr:"

	runtimeSlotIDMetadataKey         = "runtime_slot_id"
	runtimeSlotGenerationMetadataKey = "runtime_slot_generation"
)

// ErrCustodyRetenantRefused marks a move whose preconditions do not hold. The
// command changes nothing when it returns this error.
var ErrCustodyRetenantRefused = errors.New("providercontrol: managed runtime retenant refused")

// CustodyRetenantRequest names one managed runtime lease that moves to the
// person's only tenant. Apply false rolls the transaction back after every
// check and write ran (dry run).
type CustodyRetenantRequest struct {
	FromTenantID string
	ToTenantID   string
	SubjectID    string
	LeaseID      string
	Apply        bool
}

// CustodyRetenantArchive reports one archive step.
type CustodyRetenantArchive struct {
	RetenantID           string
	LeaseID              string
	ServerID             string
	StackID              string
	SlotKey              string
	ResourceGenerationID string
	Rows                 map[string]int
	ManifestDigest       string
	AlreadyArchived      bool
	Applied              bool
}

// CustodyReadmission reports one re-admission step.
type CustodyReadmission struct {
	RetenantID           string
	LeaseID              string
	ServerID             string
	ResourceGenerationID string
	RuntimeSlotID        string
	OperationID          string
	ManifestDigest       string
	HostKeyPinsRebound   int
	HeldCapacity         int
	CapacityLimit        int
	AlreadyReadmitted    bool
	Applied              bool
}

// custodyRetenantTable is one live custody table in archive order. Deletion
// runs in the same order: every table is listed before the tables it
// references. keepLive rows are archived but stay: a managed server must
// always hold its lease row (servers_runtime_target_shape and the RESTRICT
// lease foreign key), so the custody-free lease row moves with its server in
// the setup re-key and is rewritten in place by the re-admission.
type custodyRetenantTable struct {
	name      string
	predicate string // over $1 tenant, $2 lease, $3 operation ids, $4 slot ids
	keepLive  bool
}

var custodyRetenantTables = []custodyRetenantTable{
	{"provider_provision_resolution_decisions", "operation_id = ANY($3)", false},
	{"provider_provision_discovery_observations", "operation_id = ANY($3)", false},
	{"provider_operation_resource_free_terminalizations", "(lease_id = $2 OR operation_id = ANY($3))", false},
	{"managed_runtime_capacity_release_facts", "lease_id = $2", false},
	{"managed_runtime_capacity_quarantine_retirements", "lease_id = $2", false},
	{"provider_absence_observations", "operation_id = ANY($3)", false},
	{"provider_operation_evidence", "operation_id = ANY($3)", false},
	{"server_provider_resource_bindings", "lease_id = $2", false},
	{"provider_operation_execution_claims", "operation_id = ANY($3)", false},
	{"provider_async_requests", "operation_id = ANY($3)", false},
	{"provider_incidents", "(lease_id = $2 OR operation_id = ANY($3))", false},
	{"substrate_guest_leases", "lease_id = $2", false},
	{"provider_provision_dispatch_guards", "lease_id = $2", false},
	{"managed_runtime_support_replacements", "(source_lease_id = $2 OR destination_lease_id = $2)", false},
	{"managed_runtime_capacity_reservations", "lease_id = $2", false},
	{"managed_runtime_server_slot_generations", "lease_id = $2", false},
	{"managed_runtime_server_slots", "slot_id = ANY($4)", false},
	{"provider_operation_resources", "operation_id = ANY($3)", false},
	{"runtime_lease_idempotency_records", "lease_id = $2", false},
	{"provider_operation_receipts", "operation_id = ANY($3)", false},
	{"provider_operations", "lease_id = $2", false},
	{"provider_desired_spec_revisions", "lease_id = $2", false},
	{"runtime_lease_execution_authorities", "lease_id = $2", false},
	{"techstack_vm_lease_operation_journal", "lease_id = $2", false},
	{"techstack_vm_leases", "id = $2", true},
}

// Derived wake-up projections rebuilt by their triggers; legacy rows naming
// the lease are dropped, never archived.
var custodyRetenantProjections = []custodyRetenantTable{
	{"provider_provision_wait_recovery", "operation_id = ANY($3)", false},
	{"provider_stale_capacity_recovery_candidates", "lease_id = $2", false},
}

func validateCustodyRetenantRequest(req CustodyRetenantRequest) (CustodyRetenantRequest, error) {
	req.FromTenantID = strings.TrimSpace(req.FromTenantID)
	req.ToTenantID = strings.TrimSpace(req.ToTenantID)
	req.SubjectID = strings.TrimSpace(req.SubjectID)
	req.LeaseID = strings.TrimSpace(req.LeaseID)
	switch {
	case req.SubjectID == "" || strings.HasPrefix(req.SubjectID, custodyRetenantPersonPrefix):
		return req, fmt.Errorf("%w: a bare auth subject is required", ErrCustodyRetenantRefused)
	case req.ToTenantID != custodyRetenantPersonPrefix+req.SubjectID:
		return req, fmt.Errorf("%w: the target tenant must be %s%s", ErrCustodyRetenantRefused, custodyRetenantPersonPrefix, req.SubjectID)
	case req.FromTenantID == "" || req.FromTenantID == req.ToTenantID ||
		strings.HasPrefix(req.FromTenantID, custodyRetenantPersonPrefix):
		return req, fmt.Errorf("%w: the source must be a legacy tenant", ErrCustodyRetenantRefused)
	case req.LeaseID == "":
		return req, fmt.Errorf("%w: a lease id is required", ErrCustodyRetenantRefused)
	}
	return req, nil
}

// CustodyRetenantID is the deterministic id of one move.
func CustodyRetenantID(fromTenantID, toTenantID, leaseID, resourceGenerationID string) string {
	digest := prefixedSHA256(custodyRetenantDigestVersion, []byte(strings.Join([]string{
		strings.TrimSpace(fromTenantID), strings.TrimSpace(toTenantID),
		strings.TrimSpace(leaseID), strings.TrimSpace(resourceGenerationID),
	}, "\x00")))
	return "retenant-" + strings.TrimPrefix(digest, "sha256:")[:32]
}

type legacyManagedLease struct {
	ownerSubjectID string
	subjectID      string
	serverID       string
	generationID   string
	desiredState   string
	cancelled      bool
	leaseJSON      []byte
	authority      sql.NullString
	revision       sql.NullInt64
	specRevisions  int
}

// ArchiveManagedRuntimeCustody is step 1. It must run as the owner of the
// custody tables (the migration role): their immutability triggers refuse a
// DELETE, so the step disables exactly the BEFORE DELETE triggers of the
// listed tables for its own transaction and re-enables them before commit.
func ArchiveManagedRuntimeCustody(ctx context.Context, database *sql.DB, req CustodyRetenantRequest) (CustodyRetenantArchive, error) {
	req, err := validateCustodyRetenantRequest(req)
	if err != nil {
		return CustodyRetenantArchive{}, err
	}
	if database == nil {
		return CustodyRetenantArchive{}, fmt.Errorf("%w: database is required", ErrInvalidRequest)
	}
	tx, err := database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return CustodyRetenantArchive{}, fmt.Errorf("providercontrol: begin custody archive: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return CustodyRetenantArchive{}, fmt.Errorf("providercontrol: bound custody archive locks: %w", err)
	}
	if err := requireCustodyRetenantArchiveTableTx(ctx, tx); err != nil {
		return CustodyRetenantArchive{}, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, req.FromTenantID); err != nil {
		return CustodyRetenantArchive{}, fmt.Errorf("providercontrol: scope custody archive: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))
	`, req.FromTenantID, "managed-runtime-retenant:"+req.LeaseID); err != nil {
		return CustodyRetenantArchive{}, fmt.Errorf("providercontrol: lock custody archive: %w", err)
	}
	var archivedBefore bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM `+custodyRetenantArchiveTable+`
		               WHERE from_tenant_id = $1 AND to_tenant_id = $2 AND lease_id = $3)
	`, req.FromTenantID, req.ToTenantID, req.LeaseID).Scan(&archivedBefore); err != nil {
		return CustodyRetenantArchive{}, fmt.Errorf("providercontrol: inspect custody archive: %w", err)
	}
	if archivedBefore {
		return reportExistingCustodyArchiveTx(ctx, tx, req)
	}
	lease, found, err := loadLegacyManagedLeaseTx(ctx, tx, req)
	if err != nil {
		return CustodyRetenantArchive{}, err
	}
	if !found {
		return CustodyRetenantArchive{}, fmt.Errorf("%w: lease %s is not live under %s", ErrCustodyRetenantRefused, req.LeaseID, req.FromTenantID)
	}
	result, opIDs, slotIDs, err := validateLegacyManagedCustodyTx(ctx, tx, req, lease)
	if err != nil {
		return CustodyRetenantArchive{}, err
	}
	args := []any{req.FromTenantID, req.LeaseID, pq.Array(opIDs), pq.Array(slotIDs)}
	ordinal := 0
	for _, table := range custodyRetenantTables {
		var archived int
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`
			WITH source AS (
				SELECT to_jsonb(t.*) AS row_json FROM public.%s AS t
				WHERE t.tenant_id = $1 AND %s AND `+custodyRetenantTypedArgs+`
			), inserted AS (
				INSERT INTO %s (
					retenant_id, from_tenant_id, to_tenant_id, subject_id, lease_id,
					resource_generation_id, source_table, ordinal, row_json, row_digest
				)
				SELECT $5, $1, $6, $7, $2, $8::uuid, $9,
				       $10 + row_number() OVER (ORDER BY source.row_json::text),
				       source.row_json,
				       encode(sha256(convert_to(source.row_json::text, 'UTF8')), 'hex')
				FROM source
				RETURNING 1
			)
			SELECT count(*) FROM inserted
		`, pq.QuoteIdentifier(table.name), table.predicate, custodyRetenantArchiveTable),
			append(args, result.RetenantID, req.ToTenantID, req.SubjectID, result.ResourceGenerationID,
				table.name, ordinal)...,
		).Scan(&archived); err != nil {
			return CustodyRetenantArchive{}, fmt.Errorf("providercontrol: archive %s: %w", table.name, err)
		}
		result.Rows[table.name] = archived
		ordinal += archived
	}
	// Prove inside this transaction that readmit accepts exactly this archive
	// before any live custody is removed.
	if archived, err := loadCustodyRetenantArchiveTx(ctx, tx, req); err != nil {
		return CustodyRetenantArchive{}, err
	} else if err := validateReadmittableArchive(req, archived); err != nil {
		return CustodyRetenantArchive{}, err
	}
	disabled, err := disableCustodyDeleteGuardsTx(ctx, tx)
	if err != nil {
		return CustodyRetenantArchive{}, err
	}
	for _, table := range custodyRetenantTables {
		if table.keepLive {
			continue
		}
		deleted, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM public.%s AS t WHERE t.tenant_id = $1 AND %s AND `+custodyRetenantTypedArgs+``,
			pq.QuoteIdentifier(table.name), table.predicate), args...)
		if err != nil {
			return CustodyRetenantArchive{}, fmt.Errorf("providercontrol: remove archived %s: %w", table.name, err)
		}
		if rows, _ := deleted.RowsAffected(); int(rows) != result.Rows[table.name] {
			return CustodyRetenantArchive{}, fmt.Errorf("%w: %s changed during the archive", ErrCustodyRetenantRefused, table.name)
		}
	}
	for _, projection := range custodyRetenantProjections {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM public.%s AS t WHERE t.tenant_id = $1 AND %s AND `+custodyRetenantTypedArgs+``,
			pq.QuoteIdentifier(projection.name), projection.predicate), args...); err != nil {
			return CustodyRetenantArchive{}, fmt.Errorf("providercontrol: drop legacy %s: %w", projection.name, err)
		}
	}
	// Fire the deferred foreign-key checks first: a table with pending
	// trigger events cannot be altered.
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		return CustodyRetenantArchive{}, fmt.Errorf("providercontrol: validate custody archive constraints: %w", err)
	}
	if err := enableTriggersTx(ctx, tx, disabled); err != nil {
		return CustodyRetenantArchive{}, err
	}
	if result.ManifestDigest, err = custodyRetenantManifestDigestTx(ctx, tx, result.RetenantID); err != nil {
		return CustodyRetenantArchive{}, err
	}
	if !req.Apply {
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return CustodyRetenantArchive{}, fmt.Errorf("providercontrol: commit custody archive: %w", err)
	}
	result.Applied = true
	return result, nil
}

func requireCustodyRetenantArchiveTableTx(ctx context.Context, tx *sql.Tx) error {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, custodyRetenantArchiveTable).Scan(&present); err != nil {
		return fmt.Errorf("providercontrol: inspect custody archive table: %w", err)
	}
	if !present {
		return fmt.Errorf("%w: %s is missing (migration 193 is not applied)", ErrCustodyRetenantRefused, custodyRetenantArchiveTable)
	}
	return nil
}

func loadLegacyManagedLeaseTx(ctx context.Context, tx *sql.Tx, req CustodyRetenantRequest) (legacyManagedLease, bool, error) {
	var lease legacyManagedLease
	var ownerSubjectID, subjectID, serverID, generationID sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT lease.owner_subject_id, lease.subject_id, lease.server_id,
		       lease.resource_generation_id::text, lease.desired_state,
		       lease.cancelled_at IS NOT NULL, lease.lease_json, authority.execution_authority,
		       lease.lease_revision,
		       (SELECT count(*) FROM provider_desired_spec_revisions AS spec
		        WHERE spec.tenant_id = lease.tenant_id AND spec.lease_id = lease.id)
		FROM techstack_vm_leases AS lease
		LEFT JOIN runtime_lease_execution_authorities AS authority
		  ON authority.tenant_id = lease.tenant_id AND authority.lease_id = lease.id
		WHERE lease.tenant_id = $1 AND lease.id = $2
		FOR UPDATE OF lease
	`, req.FromTenantID, req.LeaseID).Scan(
		&ownerSubjectID, &subjectID, &serverID, &generationID, &lease.desiredState,
		&lease.cancelled, &lease.leaseJSON, &lease.authority, &lease.revision, &lease.specRevisions,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return legacyManagedLease{}, false, nil
	}
	if err != nil {
		return legacyManagedLease{}, false, fmt.Errorf("providercontrol: load legacy managed lease: %w", err)
	}
	lease.ownerSubjectID, lease.subjectID = ownerSubjectID.String, subjectID.String
	lease.serverID, lease.generationID = serverID.String, generationID.String
	return lease, true, nil
}

// validateLegacyManagedCustodyTx admits only a live, settled managed lease of
// the subject whose custody graph this step knows completely.
//
//nolint:gocyclo // Each refusal names one precondition of the closed move.
func validateLegacyManagedCustodyTx(
	ctx context.Context,
	tx *sql.Tx,
	req CustodyRetenantRequest,
	lease legacyManagedLease,
) (CustodyRetenantArchive, []string, []string, error) {
	refuse := func(reason string) (CustodyRetenantArchive, []string, []string, error) {
		return CustodyRetenantArchive{}, nil, nil, fmt.Errorf("%w: %s", ErrCustodyRetenantRefused, reason)
	}
	var decoded vmlease.Lease
	if err := json.Unmarshal(lease.leaseJSON, &decoded); err != nil {
		return refuse("the lease document is not readable")
	}
	switch {
	case lease.ownerSubjectID != req.SubjectID || lease.subjectID != req.SubjectID || decoded.Subject.ID != req.SubjectID:
		return refuse("the lease belongs to another subject")
	case strings.TrimSpace(decoded.Subject.OrgID) != req.FromTenantID:
		return refuse("the lease document names another tenant")
	case !lease.revision.Valid || lease.revision.Int64 != int64(vmleases.NativeAdmissionLeaseRevision) || lease.specRevisions != 1:
		return refuse("the lease has revisions after its admission; only a first-revision lease is re-admitted")
	case lease.cancelled || (lease.desiredState != string(vmlease.DesiredStateRunning) && lease.desiredState != string(vmlease.DesiredStateStopped)):
		return refuse("the lease is cancelled or not live")
	case !isHomelabProviderControlAuthority(ExecutionAuthority(lease.authority.String)):
		return refuse("the lease is not native provider-control custody")
	case lease.serverID == "" || lease.generationID == "" || vmleases.ResourceGenerationID(decoded) != lease.generationID:
		return refuse("the lease has no server or resource generation")
	case strings.TrimSpace(decoded.Metadata[vmleases.MetadataKeyDecommissionClaimDigest]) != "":
		return refuse("a decommission already claimed this generation")
	case decoded.CustodyClass == vmlease.CustodyCustomerSubstrate:
		return refuse("customer substrate leases are not managed provider custody")
	}
	result := CustodyRetenantArchive{
		RetenantID: CustodyRetenantID(req.FromTenantID, req.ToTenantID, req.LeaseID, lease.generationID),
		LeaseID:    req.LeaseID, ServerID: lease.serverID, ResourceGenerationID: lease.generationID,
		StackID: strings.TrimSpace(decoded.Metadata["stack_id"]), Rows: map[string]int{},
	}
	var serverLease, serverOwner, serverStack, lifecycle, desired sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT lease_id, owner_subject_id, stack_id, lifecycle_state, desired_state
		FROM servers WHERE tenant_id = $1 AND id = $2 FOR UPDATE
	`, req.FromTenantID, lease.serverID).Scan(&serverLease, &serverOwner, &serverStack, &lifecycle, &desired); err != nil {
		return refuse("the lease's server is not readable under the legacy tenant")
	}
	switch {
	case serverLease.String != req.LeaseID || serverOwner.String != req.SubjectID:
		return refuse("the server is bound to another lease or subject")
	case result.StackID == "" || serverStack.String != result.StackID:
		return refuse("the server and the lease name different stacks")
	case lifecycle.String == "decommissioning" || lifecycle.String == "decommissioned" || desired.String == "absent":
		return refuse("the server is being decommissioned")
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT operation_id, operation, status, phase,
		       command_json #>> '{command,resource_generation_id}'
		FROM provider_operations WHERE tenant_id = $1 AND lease_id = $2
		ORDER BY operation_id
	`, req.FromTenantID, req.LeaseID)
	if err != nil {
		return CustodyRetenantArchive{}, nil, nil, fmt.Errorf("providercontrol: load legacy operations: %w", err)
	}
	var opIDs []string
	presentProvisions := 0
	for rows.Next() {
		var id, operation, status, phase string
		var generation sql.NullString
		if err := rows.Scan(&id, &operation, &status, &phase, &generation); err != nil {
			_ = rows.Close()
			return CustodyRetenantArchive{}, nil, nil, fmt.Errorf("providercontrol: scan legacy operation: %w", err)
		}
		opIDs = append(opIDs, id)
		if !isTerminal(providerexecutor.Receipt{Status: providerexecutor.Status(status), Phase: providerexecutor.Phase(phase)}) {
			_ = rows.Close()
			return refuse("operation " + id + " is still in flight")
		}
		if operation == string(providerexecutor.OperationProvision) && status == string(providerexecutor.StatusSucceeded) &&
			phase == string(providerexecutor.PhasePresent) && generation.String == lease.generationID {
			presentProvisions++
		}
	}
	if err := rows.Close(); err != nil {
		return CustodyRetenantArchive{}, nil, nil, err
	}
	if presentProvisions != 1 {
		return refuse("the generation has no single present provision")
	}
	var activeClaims, released int
	if err := tx.QueryRowContext(ctx, `
		SELECT
		  (SELECT count(*) FROM provider_operation_execution_claims
		   WHERE tenant_id = $1 AND operation_id = ANY($2) AND state = 'active' AND lease_expires_at > clock_timestamp()),
		  (SELECT count(*) FROM managed_runtime_capacity_release_facts WHERE tenant_id = $1 AND lease_id = $3)
		  + (SELECT count(*) FROM managed_runtime_capacity_quarantine_retirements WHERE tenant_id = $1 AND lease_id = $3)
		  + (SELECT count(*) FROM provider_operation_resource_free_terminalizations WHERE tenant_id = $1 AND lease_id = $3)
	`, req.FromTenantID, pq.Array(opIDs), req.LeaseID).Scan(&activeClaims, &released); err != nil {
		return CustodyRetenantArchive{}, nil, nil, fmt.Errorf("providercontrol: inspect legacy claims: %w", err)
	}
	if activeClaims != 0 {
		return refuse("an execution claim is active on the lease")
	}
	if released != 0 {
		return refuse("the lease's capacity is already released or retired")
	}
	var slotID, slotKey, slotStack, state string
	var generations, otherSlots, otherLeasedServers int
	if err := tx.QueryRowContext(ctx, `
		SELECT g.slot_id, s.slot_key, s.stack_id, g.state,
		       (SELECT count(*) FROM managed_runtime_server_slot_generations AS other
		        WHERE other.tenant_id = g.tenant_id AND other.slot_id = g.slot_id),
		       (SELECT count(*) FROM managed_runtime_server_slots AS other
		        WHERE other.tenant_id = s.tenant_id AND other.stack_id = s.stack_id AND other.slot_id <> s.slot_id),
		       (SELECT count(*) FROM servers AS other
		        WHERE other.tenant_id = s.tenant_id AND other.stack_id = s.stack_id
		          AND other.id <> $3 AND other.lease_id IS NOT NULL)
		FROM managed_runtime_server_slot_generations AS g
		JOIN managed_runtime_server_slots AS s ON s.tenant_id = g.tenant_id AND s.slot_id = g.slot_id
		WHERE g.tenant_id = $1 AND g.lease_id = $2
		  AND g.resource_generation_id = $4::uuid AND g.runtime_server_id = $3
	`, req.FromTenantID, req.LeaseID, lease.serverID, lease.generationID).Scan(
		&slotID, &slotKey, &slotStack, &state, &generations, &otherSlots, &otherLeasedServers,
	); err != nil {
		return refuse("the generation has no single managed slot")
	}
	switch {
	case state != managedRuntimeSlotStateActive || slotStack != result.StackID:
		return refuse("the managed slot is not the active slot of the stack")
	case generations != 1:
		return refuse("the managed slot holds other generations")
	case otherSlots != 0 || otherLeasedServers != 0:
		return refuse("the stack holds other managed custody; it cannot move with this lease alone")
	}
	result.SlotKey = slotKey
	return result, opIDs, []string{slotID}, nil
}

func reportExistingCustodyArchiveTx(ctx context.Context, tx *sql.Tx, req CustodyRetenantRequest) (CustodyRetenantArchive, error) {
	archive, err := loadCustodyRetenantArchiveTx(ctx, tx, req)
	if err != nil {
		return CustodyRetenantArchive{}, err
	}
	result := CustodyRetenantArchive{
		RetenantID: archive.retenantID, LeaseID: req.LeaseID, ServerID: archive.lease.serverID,
		StackID: archive.lease.stackID, SlotKey: archive.slot.key, ResourceGenerationID: archive.generationID,
		Rows: archive.rows, ManifestDigest: archive.manifestDigest, AlreadyArchived: true,
	}
	return result, nil
}

// disableCustodyDeleteGuardsTx disables the BEFORE DELETE user triggers (the
// immutability guards) of the listed custody tables for this transaction.
// AFTER triggers stay enabled so the wake-up projections follow the removal.
func disableCustodyDeleteGuardsTx(ctx context.Context, tx *sql.Tx) ([][2]string, error) {
	names := make([]string, 0, len(custodyRetenantTables))
	for _, table := range custodyRetenantTables {
		names = append(names, "public."+table.name)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.relname, t.tgname
		FROM pg_trigger AS t JOIN pg_class AS c ON c.oid = t.tgrelid
		WHERE t.tgrelid = ANY(SELECT to_regclass(name)::oid FROM unnest($1::text[]) AS name)
		  AND NOT t.tgisinternal AND t.tgenabled <> 'D'
		  AND (t.tgtype & 2) <> 0 AND (t.tgtype & 8) <> 0
		ORDER BY c.relname, t.tgname
	`, pq.Array(names))
	if err != nil {
		return nil, fmt.Errorf("providercontrol: list custody delete guards: %w", err)
	}
	var guards [][2]string
	for rows.Next() {
		var table, trigger string
		if err := rows.Scan(&table, &trigger); err != nil {
			_ = rows.Close()
			return nil, err
		}
		guards = append(guards, [2]string{table, trigger})
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, guard := range guards {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE public.%s DISABLE TRIGGER %s`,
			pq.QuoteIdentifier(guard[0]), pq.QuoteIdentifier(guard[1]))); err != nil {
			return nil, fmt.Errorf("%w: disable %s.%s (the archive must run as the custody table owner): %v",
				ErrCustodyRetenantRefused, guard[0], guard[1], err)
		}
	}
	return guards, nil
}

func enableTriggersTx(ctx context.Context, tx *sql.Tx, guards [][2]string) error {
	for _, guard := range guards {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE public.%s ENABLE TRIGGER %s`,
			pq.QuoteIdentifier(guard[0]), pq.QuoteIdentifier(guard[1]))); err != nil {
			return fmt.Errorf("providercontrol: re-enable %s.%s: %w", guard[0], guard[1], err)
		}
	}
	return nil
}

func custodyRetenantManifestDigestTx(ctx context.Context, tx *sql.Tx, retenantID string) (string, error) {
	var digest sql.NullString
	var mismatched int
	if err := tx.QueryRowContext(ctx, `
		SELECT
		  'sha256:' || encode(sha256(convert_to(string_agg(
		      source_table || ':' || ordinal || ':' || row_digest, E'\n' ORDER BY ordinal), 'UTF8')), 'hex'),
		  count(*) FILTER (WHERE row_digest <> encode(sha256(convert_to(row_json::text, 'UTF8')), 'hex'))
		FROM `+custodyRetenantArchiveTable+` WHERE retenant_id = $1
	`, retenantID).Scan(&digest, &mismatched); err != nil {
		return "", fmt.Errorf("providercontrol: digest custody archive: %w", err)
	}
	if !digest.Valid || mismatched != 0 {
		return "", fmt.Errorf("%w: the custody archive %s is empty or altered", ErrCustodyRetenantRefused, retenantID)
	}
	return digest.String, nil
}

// archivedManagedCustody is the part of an archive the re-admission needs.
type archivedManagedCustody struct {
	retenantID     string
	generationID   string
	manifestDigest string
	rows           map[string]int
	lease          struct {
		document       vmlease.Lease
		documentJSON   json.RawMessage
		serverID       string
		stackID        string
		ownerSubjectID string
	}
	operation struct {
		idempotencyKey string
	}
	desired struct {
		ref     string
		payload json.RawMessage
	}
	reservation struct {
		mode   CapacityMode
		limit  int
		source string
	}
	slot struct {
		key     string
		stackID string
	}
}

func loadCustodyRetenantArchiveTx(ctx context.Context, tx *sql.Tx, req CustodyRetenantRequest) (archivedManagedCustody, error) {
	var archive archivedManagedCustody
	rows, err := tx.QueryContext(ctx, `
		SELECT retenant_id, subject_id, resource_generation_id::text, source_table, row_json
		FROM `+custodyRetenantArchiveTable+`
		WHERE from_tenant_id = $1 AND to_tenant_id = $2 AND lease_id = $3
		ORDER BY retenant_id, ordinal
	`, req.FromTenantID, req.ToTenantID, req.LeaseID)
	if err != nil {
		return archive, fmt.Errorf("providercontrol: load custody archive: %w", err)
	}
	defer func() { _ = rows.Close() }()
	archive.rows = map[string]int{}
	var leaseRows, operationRows, specRows, reservationRows, slotRows int
	for rows.Next() {
		var retenantID, subjectID, generationID, table string
		var row json.RawMessage
		if err := rows.Scan(&retenantID, &subjectID, &generationID, &table, &row); err != nil {
			return archive, fmt.Errorf("providercontrol: scan custody archive: %w", err)
		}
		if archive.retenantID == "" {
			archive.retenantID, archive.generationID = retenantID, generationID
		}
		if retenantID != archive.retenantID || subjectID != req.SubjectID ||
			retenantID != CustodyRetenantID(req.FromTenantID, req.ToTenantID, req.LeaseID, generationID) {
			return archive, fmt.Errorf("%w: the custody archive of %s is ambiguous or belongs to another subject", ErrCustodyRetenantRefused, req.LeaseID)
		}
		archive.rows[table]++
		if err := archive.absorb(table, row, &leaseRows, &operationRows, &specRows, &reservationRows, &slotRows); err != nil {
			return archive, err
		}
	}
	if err := rows.Err(); err != nil {
		return archive, err
	}
	if archive.retenantID == "" {
		return archive, fmt.Errorf("%w: lease %s is neither live under %s nor archived for %s", ErrCustodyRetenantRefused, req.LeaseID, req.FromTenantID, req.ToTenantID)
	}
	if leaseRows != 1 || operationRows < 1 || specRows < 1 || reservationRows != 1 || slotRows != 1 ||
		archive.operation.idempotencyKey == "" || archive.desired.ref == "" {
		return archive, fmt.Errorf("%w: the custody archive of %s is incomplete", ErrCustodyRetenantRefused, req.LeaseID)
	}
	if archive.manifestDigest, err = custodyRetenantManifestDigestTx(ctx, tx, archive.retenantID); err != nil {
		return archive, err
	}
	return archive, nil
}

func (archive *archivedManagedCustody) absorb(
	table string,
	row json.RawMessage,
	leaseRows, operationRows, specRows, reservationRows, slotRows *int,
) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row, &fields); err != nil {
		return fmt.Errorf("%w: archived %s row is not an object", ErrCustodyRetenantRefused, table)
	}
	text := func(name string) string {
		var value string
		_ = json.Unmarshal(fields[name], &value)
		return value
	}
	switch table {
	case "techstack_vm_leases":
		*leaseRows++
		var document vmlease.Lease
		if err := json.Unmarshal(fields["lease_json"], &document); err != nil {
			return fmt.Errorf("%w: archived lease document is not readable", ErrCustodyRetenantRefused)
		}
		archive.lease.document, archive.lease.documentJSON = document, fields["lease_json"]
		archive.lease.serverID, archive.lease.ownerSubjectID = text("server_id"), text("owner_subject_id")
		archive.lease.stackID = strings.TrimSpace(document.Metadata["stack_id"])
	case "provider_operations":
		*operationRows++
		var envelope struct {
			Command struct {
				ResourceGenerationID string `json:"resource_generation_id"`
			} `json:"command"`
		}
		_ = json.Unmarshal(fields["command_json"], &envelope)
		// Only the generation's one present provision names the admission key.
		if text("operation") == string(providerexecutor.OperationProvision) &&
			text("status") == string(providerexecutor.StatusSucceeded) &&
			text("phase") == string(providerexecutor.PhasePresent) &&
			envelope.Command.ResourceGenerationID == archive.generationID {
			if archive.operation.idempotencyKey != "" {
				return fmt.Errorf("%w: the archive holds two present provisions of the generation", ErrCustodyRetenantRefused)
			}
			archive.operation.idempotencyKey = text("idempotency_key")
		}
	case "provider_desired_spec_revisions":
		*specRows++
		var revision int64
		_ = json.Unmarshal(fields["revision"], &revision)
		if revision == int64(vmleases.NativeAdmissionLeaseRevision) {
			archive.desired.ref, archive.desired.payload = text("spec_ref"), fields["spec_json"]
		}
	case "managed_runtime_capacity_reservations":
		*reservationRows++
		archive.reservation.mode, archive.reservation.source = CapacityMode(text("reservation_mode")), text("policy_source")
		var limit sql.NullInt64
		if raw := fields["capacity_limit"]; len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &limit.Int64); err != nil {
				return fmt.Errorf("%w: archived capacity limit is not readable", ErrCustodyRetenantRefused)
			}
		}
		archive.reservation.limit = int(limit.Int64)
		if text("reservation_origin") != managedRuntimeCapacityOrigin {
			return fmt.Errorf("%w: the archived reservation is not native admission custody", ErrCustodyRetenantRefused)
		}
	case "managed_runtime_server_slots":
		*slotRows++
		archive.slot.key, archive.slot.stackID = text("slot_key"), text("stack_id")
	}
	return nil
}

// CustodyReadmitRequest is step 2.
type CustodyReadmitRequest = CustodyRetenantRequest

// ReadmitArchivedManagedRuntime is step 2: after the setup-row re-key moved
// the lease's server and stack to usr:<sub>, it admits the archived lease and
// generation there and parks the provision for the automatic resolver. It
// performs no provider call. The capacity reservation carries the archived
// grant and the database still enforces its limit under the new tenant.
//
//nolint:gocyclo,funlen // One transaction: the order is the admission order.
func (a *NativeAdmission) ReadmitArchivedManagedRuntime(ctx context.Context, req CustodyReadmitRequest) (CustodyReadmission, error) {
	ctx = SystemAuthorityContext(ctx)
	req, err := validateCustodyRetenantRequest(req)
	if err != nil {
		return CustodyReadmission{}, err
	}
	if !a.configured() {
		return CustodyReadmission{}, fmt.Errorf("%w: native admission is not configured", ErrInvalidRequest)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return CustodyReadmission{}, fmt.Errorf("providercontrol: begin readmission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return CustodyReadmission{}, fmt.Errorf("providercontrol: bound readmission locks: %w", err)
	}
	if err := requireCustodyRetenantArchiveTableTx(ctx, tx); err != nil {
		return CustodyReadmission{}, err
	}
	archive, err := loadCustodyRetenantArchiveTx(ctx, tx, req)
	if err != nil {
		return CustodyReadmission{}, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, req.ToTenantID); err != nil {
		return CustodyReadmission{}, fmt.Errorf("providercontrol: scope readmission: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))
	`, req.ToTenantID, "managed-runtime-retenant:"+req.LeaseID); err != nil {
		return CustodyReadmission{}, fmt.Errorf("providercontrol: lock readmission: %w", err)
	}
	legacy := archive.lease.document
	result := CustodyReadmission{
		RetenantID: archive.retenantID, LeaseID: req.LeaseID, ServerID: archive.lease.serverID,
		ResourceGenerationID: archive.generationID, ManifestDigest: archive.manifestDigest,
		RuntimeSlotID: DeriveManagedRuntimeSlotID(req.ToTenantID, archive.lease.stackID, archive.slot.key),
	}
	if err := validateReadmittableArchive(req, archive); err != nil {
		return CustodyReadmission{}, err
	}
	var serverLease, serverOwner, serverStack, providerRef, lifecycle, desired, serverName sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT lease_id, owner_subject_id, stack_id, provider_ref, lifecycle_state, desired_state, name
		FROM servers WHERE tenant_id = $1 AND id = $2 FOR UPDATE
	`, req.ToTenantID, archive.lease.serverID).Scan(&serverLease, &serverOwner, &serverStack, &providerRef, &lifecycle, &desired, &serverName)
	if errors.Is(err, sql.ErrNoRows) {
		return CustodyReadmission{}, fmt.Errorf("%w: server %s is not under %s yet; run the setup re-key first", ErrCustodyRetenantRefused, archive.lease.serverID, req.ToTenantID)
	}
	if err != nil {
		return CustodyReadmission{}, fmt.Errorf("providercontrol: load the moved server: %w", err)
	}
	var readmitted bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM runtime_lease_execution_authorities WHERE tenant_id = $1 AND lease_id = $2)
	`, req.ToTenantID, req.LeaseID).Scan(&readmitted); err != nil {
		return CustodyReadmission{}, fmt.Errorf("providercontrol: inspect readmitted custody: %w", err)
	}
	if readmitted {
		return a.reportExistingReadmissionTx(ctx, tx, req, result)
	}
	switch {
	case serverLease.String != req.LeaseID:
		return CustodyReadmission{}, fmt.Errorf("%w: the moved server is not bound to %s", ErrCustodyRetenantRefused, req.LeaseID)
	case serverOwner.String != req.SubjectID || serverStack.String != archive.lease.stackID:
		return CustodyReadmission{}, fmt.Errorf("%w: the moved server belongs to another subject or stack", ErrCustodyRetenantRefused)
	case !strings.EqualFold(strings.TrimSpace(providerRef.String), legacy.Resource.ProviderID):
		return CustodyReadmission{}, fmt.Errorf("%w: the moved server names another provider", ErrCustodyRetenantRefused)
	case lifecycle.String == "decommissioning" || lifecycle.String == "decommissioned" || desired.String == "absent":
		return CustodyReadmission{}, fmt.Errorf("%w: the moved server is being decommissioned", ErrCustodyRetenantRefused)
	}

	lease := cloneAdmissionLease(legacy)
	lease.Subject.OrgID = req.ToTenantID
	lease.Resource.EngineVMID, lease.Resource.VMID, lease.Resource.SimulationID = "", "", ""
	lease.ValidFrom, lease.ValidUntil, lease.RenewedAt, lease.CancelledAt = time.Time{}, time.Time{}, time.Time{}, nil
	delete(lease.Metadata, vmleases.MetadataKeyResourceGenerationID)
	lease.Metadata[runtimeSlotIDMetadataKey] = result.RuntimeSlotID
	lease.Metadata[runtimeSlotGenerationMetadataKey] = "1"
	request := NativeProvisionAdmissionRequest{
		TenantID: req.ToTenantID, RuntimeSlotKey: archive.slot.key, RuntimeSlotID: result.RuntimeSlotID,
		RuntimeSlotGeneration: 1, RuntimeServerID: archive.lease.serverID, OwnerSubjectID: req.SubjectID,
		IdempotencyKey: archive.operation.idempotencyKey, Lease: lease,
		Server:         NativeAdmissionServer{StackID: archive.lease.stackID, Name: serverName.String},
		DesiredSpecRef: archive.desired.ref, DesiredSpec: archive.desired.payload,
	}
	request, _, _, err = normalizeNativeProvisionAdmission(request)
	if err != nil {
		return CustodyReadmission{}, err
	}
	requestDigest, err := readmissionRequestDigest(request, archive)
	if err != nil {
		return CustodyReadmission{}, err
	}
	admitted, err := a.leases.ReadmitRetenantedLeaseTx(ctx, tx, vmleases.NativeReadmissionRequest{
		Lease: request.Lease, OwnerSubjectID: req.SubjectID, ServerID: archive.lease.serverID,
		ValidFor: request.ValidFor, ArchivedLeaseJSON: archive.lease.documentJSON,
		ArchivedResourceGenerationID: archive.generationID,
	})
	if err != nil {
		return CustodyReadmission{}, fmt.Errorf("providercontrol: readmit lease: %w", err)
	}
	if err := ensureManagedCredentialAuthorityTx(
		ctx, tx, req.ToTenantID, admitted.Lease, admitted.DatabaseTime, a.managedCredentialAuthorities,
	); err != nil {
		return CustodyReadmission{}, err
	}
	prepared, err := a.coordinator.prepareStartTx(ctx, tx, StartRequest{
		TenantID: req.ToTenantID, LeaseID: req.LeaseID, LeaseRevision: admitted.LeaseRevision,
		RuntimeServerID: archive.lease.serverID, ResourceGenerationID: admitted.ResourceGenerationID,
		Operation: providerexecutor.OperationProvision, IdempotencyKey: request.IdempotencyKey,
		LedgerRevision: nativeInitialLedgerRevision, RequestedAt: admitted.DatabaseTime,
		DesiredSpec: &DesiredSpecRevision{
			TenantID: req.ToTenantID, LeaseID: req.LeaseID, Revision: admitted.LeaseRevision,
			Ref: request.DesiredSpecRef, Digest: sha256Digest(request.DesiredSpec),
			Payload: request.DesiredSpec, CreatedAt: admitted.DatabaseTime,
		},
	})
	if err != nil {
		return CustodyReadmission{}, err
	}
	if prepared.command.ProviderID != admitted.Lease.Resource.ProviderID ||
		prepared.provisionDispatch != ProvisionDispatchAtMostOnceManualReconcile {
		return CustodyReadmission{}, fmt.Errorf("%w: the provider profile is not an at-most-once provision of %s",
			ErrCustodyRetenantRefused, admitted.Lease.Resource.ProviderID)
	}
	if err := insertNativeExecutionAuthorityTx(ctx, tx, req.ToTenantID, req.LeaseID, admitted.DatabaseTime); err != nil {
		return CustodyReadmission{}, err
	}
	record, created, err := a.ledger.beginOperationInTx(
		ctx, tx, ExecutionAuthorityHomelabProviderControl, prepared.profileSnapshot,
		prepared.provisionDispatch, prepared.command, prepared.initial, prepared.desiredSpec,
	)
	if err != nil {
		return CustodyReadmission{}, err
	}
	if !created {
		return CustodyReadmission{}, fmt.Errorf("%w: provider operation already exists without readmission custody", ErrNativeAdmissionConflict)
	}
	result.OperationID = record.Command.OperationID
	if err := insertNativeIdempotencyTx(
		ctx, tx, req.ToTenantID, request.IdempotencyKey, requestDigest[:], req.LeaseID, record.Command.OperationID,
	); err != nil {
		return CustodyReadmission{}, err
	}
	admission := NativeProvisionAdmissionResult{
		RuntimeSlotKey: request.RuntimeSlotKey, RuntimeSlotID: request.RuntimeSlotID, RuntimeSlotGeneration: 1,
		LeaseID: req.LeaseID, LeaseRevision: admitted.LeaseRevision, RuntimeServerID: archive.lease.serverID,
		ResourceGenerationID: admitted.ResourceGenerationID, Operation: record,
		RequestDigest: "sha256:" + hex.EncodeToString(requestDigest[:]), Created: true,
	}
	grant, err := archivedCapacityGrant(req.ToTenantID, req.SubjectID, admitted.Lease.Resource.ProviderID, archive)
	if err != nil {
		return CustodyReadmission{}, err
	}
	if err := insertManagedRuntimeCapacityReservationTx(ctx, tx, request, admission, grant, admitted.DatabaseTime); err != nil {
		return CustodyReadmission{}, err
	}
	if err := insertManagedRuntimeServerSlotTx(ctx, tx, request, admission, admitted.DatabaseTime); err != nil {
		return CustodyReadmission{}, err
	}
	if err := a.parkReadmittedProvisionTx(ctx, tx, record, archive.manifestDigest); err != nil {
		return CustodyReadmission{}, err
	}
	if result.HostKeyPinsRebound, err = rebindRuntimeHostKeyPinsTx(ctx, tx, req, admitted.Lease); err != nil {
		return CustodyReadmission{}, err
	}
	if result.HeldCapacity, err = countHeldManagedRuntimeCapacity(ctx, tx, req.ToTenantID, req.SubjectID); err != nil {
		return CustodyReadmission{}, err
	}
	result.CapacityLimit = grant.Limit
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		return CustodyReadmission{}, fmt.Errorf("providercontrol: validate readmission constraints: %w", err)
	}
	if !req.Apply {
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return CustodyReadmission{}, fmt.Errorf("providercontrol: commit readmission: %w", err)
	}
	result.Applied = true
	return result, nil
}

// validateReadmittableArchive is the one archive check archive and readmit
// share, so archive never commits a removal readmit would refuse.
func validateReadmittableArchive(req CustodyRetenantRequest, archive archivedManagedCustody) error {
	legacy := archive.lease.document
	switch {
	case archive.lease.ownerSubjectID != req.SubjectID || legacy.Subject.ID != req.SubjectID:
		return fmt.Errorf("%w: the archived lease belongs to another subject", ErrCustodyRetenantRefused)
	case archive.slot.stackID != archive.lease.stackID || archive.lease.serverID == "" ||
		!managedRuntimeSlotKeyPattern.MatchString(archive.slot.key):
		return fmt.Errorf("%w: the archived slot, lease and server disagree", ErrCustodyRetenantRefused)
	case legacy.Metadata == nil || strings.TrimSpace(legacy.Resource.ProviderID) == "" || len(archive.lease.documentJSON) == 0:
		return fmt.Errorf("%w: the archived lease document is incomplete", ErrCustodyRetenantRefused)
	}
	_, err := archivedCapacityGrant(req.ToTenantID, req.SubjectID, legacy.Resource.ProviderID, archive)
	return err
}

func readmissionRequestDigest(request NativeProvisionAdmissionRequest, archive archivedManagedCustody) ([sha256.Size]byte, error) {
	payload, err := marshalNativeAdmissionDigest(request)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256([]byte(custodyRetenantAdmissionDigest + "\x00" + archive.retenantID + "\x00" +
		archive.manifestDigest + "\x00" + string(payload))), nil
}

func archivedCapacityGrant(tenantID, subjectID, providerID string, archive archivedManagedCustody) (normalizedCapacityGrant, error) {
	grant := CapacityGrant{
		ScopeKind: managedRuntimeCapacityScopeOwner, ScopeID: subjectID,
		Mode: archive.reservation.mode, Limit: archive.reservation.limit, DecisionSource: archive.reservation.source,
	}
	if !validNativeCapacityDecisionSource(grant.DecisionSource) ||
		(grant.Mode == CapacityModeLimited && (grant.Limit <= 0 || grant.Limit > managedRuntimeCapacityMaxLimit)) ||
		(grant.Mode == CapacityModeUnlimited && grant.Limit != 0) ||
		(grant.Mode != CapacityModeLimited && grant.Mode != CapacityModeUnlimited) {
		return normalizedCapacityGrant{}, fmt.Errorf("%w: the archived capacity grant is not a native admission grant", ErrCustodyRetenantRefused)
	}
	return normalizedCapacityGrant{
		CapacityGrant: grant,
		PolicyDigest: managedRuntimeCapacityPolicyDigest(
			tenantID, strings.ToLower(strings.TrimSpace(providerID)), grant.ScopeKind, grant.ScopeID,
			grant.Mode, grant.Limit, grant.DecisionSource,
		),
	}, nil
}

// parkReadmittedProvisionTx appends the coordinator-owned accepted receipt and
// takes the at-most-once dispatch decision for the generation without calling
// the provider: the guard binds the archive instead of a prepared create
// request, and the claim is released at once. The operation is therefore
// manual-reconcile from its first committed state, and the automatic resolver
// adopts the provider resources the archived custody created.
func (a *NativeAdmission) parkReadmittedProvisionTx(
	ctx context.Context,
	tx *sql.Tx,
	record OperationRecord,
	manifestDigest string,
) error {
	if record.Head.Phase != providerexecutor.PhaseRequested {
		return fmt.Errorf("%w: readmitted provision must start at requested", ErrCustodyRetenantRefused)
	}
	accepted, err := providerexecutor.AssembleReceipt(
		ctx,
		providerexecutor.ExecutionRequest{Command: record.Command, Previous: record.Head},
		providerexecutor.ExecutionResult{Status: providerexecutor.StatusPending, Phase: providerexecutor.PhaseAccepted},
		a.coordinator.now(), a.ledger.verifier,
	)
	if err != nil {
		return err
	}
	if err := providerexecutor.ValidateAppend(ctx, record.Command, record.Head, accepted, a.ledger.verifier); err != nil {
		return err
	}
	acceptedJSON, err := json.Marshal(accepted)
	if err != nil {
		return fmt.Errorf("providercontrol: encode readmission receipt: %w", err)
	}
	if err := a.ledger.appendReceiptTx(ctx, tx, record.Command, record.Head, accepted, acceptedJSON); err != nil {
		return err
	}
	token, err := executionClaimToken()
	if err != nil {
		return err
	}
	claim, ttl, err := prepareExecutionClaim(
		ctx, record.Command, accepted, ExecutionClaimSideEffecting, custodyRetenantClaimOwner, token, 0, a.ledger.verifier,
	)
	if err != nil {
		return err
	}
	invocation, err := newAdapterInvocation(providerexecutor.ExecutionRequest{Command: record.Command, Previous: accepted})
	if err != nil {
		return err
	}
	binding := PreparedProvisionBinding{
		RequestDigest: prefixedSHA256(custodyRetenantAdoptionVersion,
			[]byte(record.Command.TenantID+"\x00"+record.Command.LeaseID+"\x00"+
				record.Command.ResourceGenerationID+"\x00"+manifestDigest)),
		CredentialVersionHash: record.Command.CustodyHash,
		ProviderScopeHash:     record.Command.ConnectionHash,
		CorrelationHash:       sha256Digest([]byte(invocation.CorrelationID)),
		AdapterManifestHash:   record.ExecutionProfile.AdapterManifestHash,
	}
	if err := a.ledger.acquireProvisionDispatchClaimTx(ctx, tx, record.Command, accepted, binding, invocation, &claim, ttl); err != nil {
		return err
	}
	released, err := tx.ExecContext(ctx, `
		UPDATE provider_operation_execution_claims
		SET state = 'released', released_at = clock_timestamp()
		WHERE tenant_id = $1 AND operation_id = $2 AND head_receipt_digest = $3
		  AND claim_token_digest = $4 AND claim_owner = $5 AND state = 'active'
	`, claim.TenantID, claim.OperationID, claim.HeadReceiptDigest, executionClaimTokenDigest(claim.Token), claim.Owner)
	if err != nil {
		return fmt.Errorf("providercontrol: release the readmission claim: %w", normalizeLeaseFenceDatabaseError(err))
	}
	if rows, _ := released.RowsAffected(); rows != 1 {
		return ErrExecutionClaimLost
	}
	return nil
}

// rebindRuntimeHostKeyPinsTx moves the lease-bound SSH host-key pin of the
// server to the new tenant's lease binding; the pinned fingerprint is kept.
func rebindRuntimeHostKeyPinsTx(ctx context.Context, tx *sql.Tx, req CustodyRetenantRequest, lease vmlease.Lease) (int, error) {
	updated, err := tx.ExecContext(ctx, `
		UPDATE runtime_host_key_pins
		SET lease_binding = runtime_host_key_pin_lease_binding($2, $3)
		WHERE lease_binding = runtime_host_key_pin_lease_binding($1, $3)
		  AND split_part(address, ':', 1) = ANY($4)
	`, req.FromTenantID, req.ToTenantID, req.LeaseID, pq.Array(leaseHostAddresses(lease)))
	if err != nil {
		return 0, fmt.Errorf("providercontrol: rebind runtime host key pins: %w", err)
	}
	rows, _ := updated.RowsAffected()
	return int(rows), nil
}

func leaseHostAddresses(lease vmlease.Lease) []string {
	var hosts []string
	for _, key := range []string{"runtime_public_ip", "runtime_ssh_host", "node_public_ip", "public_ip"} {
		if value := strings.ToLower(strings.TrimSpace(lease.Metadata[key])); value != "" {
			hosts = append(hosts, value)
		}
	}
	return hosts
}

func (a *NativeAdmission) reportExistingReadmissionTx(
	ctx context.Context,
	tx *sql.Tx,
	req CustodyRetenantRequest,
	result CustodyReadmission,
) (CustodyReadmission, error) {
	var generationID, operationID string
	if err := tx.QueryRowContext(ctx, `
		SELECT lease.resource_generation_id::text, guard.operation_id
		FROM techstack_vm_leases AS lease
		JOIN provider_provision_dispatch_guards AS guard
		  ON guard.tenant_id = lease.tenant_id AND guard.lease_id = lease.id
		 AND guard.resource_generation_id = lease.resource_generation_id
		WHERE lease.tenant_id = $1 AND lease.id = $2
	`, req.ToTenantID, req.LeaseID).Scan(&generationID, &operationID); err != nil {
		return CustodyReadmission{}, fmt.Errorf("%w: the server is bound to %s but its readmitted custody is incomplete", ErrCustodyRetenantRefused, req.LeaseID)
	}
	if generationID != result.ResourceGenerationID {
		return CustodyReadmission{}, fmt.Errorf("%w: the live lease holds another generation", ErrCustodyRetenantRefused)
	}
	result.OperationID, result.AlreadyReadmitted = operationID, true
	return result, nil
}

// CustodyRetenantStatus is the read-only view the operator follows between
// the steps and while the resolver adopts the provider resources.
type CustodyRetenantStatus struct {
	LiveUnderFrom      bool // legacy provider custody (operations) still live
	Archived           bool
	ArchiveRows        int
	ManifestDigest     string
	ReadmittedUnderTo  bool
	OperationID        string
	OperationStatus    string
	OperationPhase     string
	ResolutionOutcome  string
	ServerUnderTo      bool
	ServerLeaseBinding string
}

// ManagedRuntimeRetenantStatus reads where the lease's custody is. It writes
// nothing.
func ManagedRuntimeRetenantStatus(ctx context.Context, database *sql.DB, req CustodyRetenantRequest) (CustodyRetenantStatus, error) {
	req, err := validateCustodyRetenantRequest(req)
	if err != nil {
		return CustodyRetenantStatus{}, err
	}
	var status CustodyRetenantStatus
	var archivePresent bool
	read := func(tenantID string, fn func(*sql.Tx) error) error {
		tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
			return err
		}
		return fn(tx)
	}
	if err := read(req.FromTenantID, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM provider_operations WHERE tenant_id = $1 AND lease_id = $2)`,
			req.FromTenantID, req.LeaseID).Scan(&status.LiveUnderFrom); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, custodyRetenantArchiveTable).Scan(&archivePresent); err != nil || !archivePresent {
			return err
		}
		var retenantID sql.NullString
		if err := tx.QueryRowContext(ctx, `
			SELECT min(retenant_id), count(*) FROM `+custodyRetenantArchiveTable+`
			WHERE from_tenant_id = $1 AND to_tenant_id = $2 AND lease_id = $3
		`, req.FromTenantID, req.ToTenantID, req.LeaseID).Scan(&retenantID, &status.ArchiveRows); err != nil {
			return err
		}
		if status.Archived = status.ArchiveRows > 0; status.Archived {
			status.ManifestDigest, err = custodyRetenantManifestDigestTx(ctx, tx, retenantID.String)
		}
		return err
	}); err != nil {
		return CustodyRetenantStatus{}, fmt.Errorf("providercontrol: read legacy retenant status: %w", err)
	}
	if err := read(req.ToTenantID, func(tx *sql.Tx) error {
		var operationID, opStatus, phase, outcome, serverLease sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT operation.operation_id, operation.status, operation.phase,
			       (SELECT decision.outcome FROM provider_provision_resolution_decisions AS decision
			        WHERE decision.tenant_id = operation.tenant_id AND decision.operation_id = operation.operation_id
			        ORDER BY decision.resolution_revision DESC LIMIT 1)
			FROM techstack_vm_leases AS lease
			JOIN provider_operations AS operation
			  ON operation.tenant_id = lease.tenant_id AND operation.lease_id = lease.id
			 AND operation.operation = 'provision'
			WHERE lease.tenant_id = $1 AND lease.id = $2
			ORDER BY operation.created_at DESC LIMIT 1
		`, req.ToTenantID, req.LeaseID).Scan(&operationID, &opStatus, &phase, &outcome)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		status.ReadmittedUnderTo = operationID.Valid
		status.OperationID, status.OperationStatus = operationID.String, opStatus.String
		status.OperationPhase, status.ResolutionOutcome = phase.String, outcome.String
		if !archivePresent {
			return nil
		}
		err = tx.QueryRowContext(ctx, `
			SELECT server.lease_id FROM servers AS server
			WHERE server.tenant_id = $1 AND server.id IN (
			    SELECT row_json->>'server_id' FROM `+custodyRetenantArchiveTable+`
			    WHERE to_tenant_id = $1 AND lease_id = $2 AND source_table = 'techstack_vm_leases')
		`, req.ToTenantID, req.LeaseID).Scan(&serverLease)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		status.ServerUnderTo, status.ServerLeaseBinding = err == nil, serverLease.String
		return err
	}); err != nil {
		return CustodyRetenantStatus{}, fmt.Errorf("providercontrol: read canonical retenant status: %w", err)
	}
	return status, nil
}

// custodyRetenantTypedArgs types every selection parameter, so a predicate
// that does not use one still prepares.
const custodyRetenantTypedArgs = "$2::text IS NOT NULL AND $3::text[] IS NOT NULL AND $4::text[] IS NOT NULL"
