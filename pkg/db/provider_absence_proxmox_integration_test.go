package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	providerexecutor "github.com/kombifyio/stackkits/contracts/providerexecutor/v1beta1"

	"github.com/kombifyio/homelab/internal/providerevidence"
)

// Sensitive migration boundary (kombify-StackKits-8t7b): migration 186 admits
// the Proxmox substrate adapter's absence observation. Before it, the recorder's
// insert violated provider_absence_observations_provider_id_check and a Proxmox
// decommission could never reach absent. The observation goes through the real
// recorder and verifier exactly as internal/substrateruntime records it.
func TestIntegrationProxmoxAbsenceObservationAdmitted(t *testing.T) {
	dsn := integrationDSN()
	if dsn == "" {
		t.Skip("HOMELAB_TEST_POSTGRES_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	target, _, _ := newForwardBatchDatabase(t, ctx, dsn)
	pinApplyMigrationsBefore(t, ctx, target, "186_")

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	generation := uuid.NewString()
	digest := func(seed string) string { return "sha256:" + strings.Repeat(seed, 64) }
	command := providerexecutor.Command{
		APIVersion: "homelab.provider-executor/v1beta1", OperationID: "op-proxmox-" + suffix,
		Operation: providerexecutor.OperationDecommission, TenantID: "absence-tenant-" + suffix,
		LeaseID: "lease-" + suffix, LeaseRevision: 3, RuntimeServerID: "server-" + suffix,
		ResourceGenerationID: generation, IdempotencyKey: "idem-" + suffix,
		ProviderID: "proxmox", AdapterID: "proxmox-substrate-v1",
		ConnectionRef:  "provider-connection://substrate/worker-" + suffix + "/lease-" + suffix,
		ConnectionHash: digest("1"), CapabilitySnapshotHash: digest("2"), ExecutionProfileHash: digest("3"),
		ResourceGraphHash: digest("4"), LedgerRevision: 5, CommandDigest: digest("5"),
		RequestedAt: time.Now().UTC(),
	}
	resource := providerexecutor.ResourceTarget{
		BindingID: "guest-" + generation, Kind: "virtual_machine", NativeRef: "proxmox://node-a/qemu/9101",
		OwnershipHash: digest("6"), Disposition: providerexecutor.DispositionDelete,
	}
	observed := providerevidence.Observation{
		Endpoint: "proxmox-node-local:/qemu", StatusCode: 200,
		CollectedAt: time.Now().UTC(), AuthoritativeListAbsent: resource.NativeRef,
	}
	recorder, err := providerevidence.NewRecorder(target.DB)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := providerevidence.NewVerifier(target.DB)
	if err != nil {
		t.Fatal(err)
	}

	// The defect: through migration 185 the store refuses the observation the
	// recorder itself accepts, so the adapter can only report pending.
	if _, err := recorder.Record(ctx, command, resource, observed); !errors.Is(err, providerevidence.ErrAbsenceObservationUnavailable) ||
		!strings.Contains(err.Error(), "provider_absence_observations_provider_id_check") {
		t.Fatalf("pre-186 Proxmox absence record error = %v", err)
	}

	pinApplyMigration(t, ctx, target, "186_proxmox_absence_observations.sql")

	evidence, err := recorder.Record(ctx, command, resource, observed)
	if err != nil {
		t.Fatalf("record Proxmox absence: %v", err)
	}
	if evidence.ProviderID != "proxmox" || evidence.Observation != providerexecutor.ObservationAbsent || !evidence.Definitive {
		t.Fatalf("Proxmox absence evidence = %+v", evidence)
	}
	if err := verifier.VerifyEvidence(ctx, command, resource, evidence); err != nil {
		t.Fatalf("verify Proxmox absence: %v", err)
	}
	// A retried teardown re-reads the node and adopts the first observation.
	retried := observed
	retried.CollectedAt = observed.CollectedAt.Add(time.Minute)
	again, err := recorder.Record(ctx, command, resource, retried)
	if err != nil || again.Digest != evidence.Digest || !again.CollectedAt.Equal(evidence.CollectedAt) {
		t.Fatalf("retried Proxmox absence = %+v, %v", again, err)
	}
	tx := pinBeginInTenant(t, ctx, target.DB, command.TenantID)
	var providerID string
	if err := tx.QueryRowContext(ctx, `SELECT provider_id FROM provider_absence_observations WHERE tenant_id = $1 AND operation_id = $2`,
		command.TenantID, command.OperationID).Scan(&providerID); err != nil || providerID != "proxmox" {
		t.Fatalf("stored provider = %q, %v", providerID, err)
	}
	_ = tx.Rollback()

	// Widening only: a provider outside the admitted set is still refused.
	tx = pinBeginInTenant(t, ctx, target.DB, command.TenantID)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO provider_absence_observations
		(tenant_id, operation_id, binding_id, provider_id, evidence_ref, evidence_digest, attestation_ref, attestation_digest,
		 observation, source, subject_hash, native_ref_hash, collected_at, document_json)
		VALUES ($1, 'op-other', 'server', 'hetzner', 'provider-evidence://other', $2, 'provider-attestation://other', $2,
		        'absent', 'provider_api', $2, $2, now(), '{}'::jsonb)`, command.TenantID, digest("7"))
	var refusal *pgconn.PgError
	if !errors.As(err, &refusal) || refusal.Code != "23514" || refusal.ConstraintName != "provider_absence_observations_provider_id_check" {
		t.Fatalf("unadmitted provider insert error = %v", err)
	}
}
