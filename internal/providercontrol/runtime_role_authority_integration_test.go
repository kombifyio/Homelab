package providercontrol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kombifyio/homelab/internal/runtimeproduct/serverruntime"
	"github.com/kombifyio/homelab/internal/runtimeproduct/vmlease"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
	providerexecutor "github.com/kombifyio/stackkits/contracts/providerexecutor/v1beta1"
)

func TestNativeAdmissionIntegrationRestrictedRoleUsesOnlyBoundedTenantDiscovery(t *testing.T) {
	adminDatabase := openNativeAdmissionIntegrationDB(t)
	seedNativeAdmissionCredentialHandle(t, adminDatabase)
	runtimeDatabase := openRestrictedNativeAdmissionRuntimeDB(t, adminDatabase)

	var runtimeRole, schemaName string
	if err := runtimeDatabase.QueryRowContext(t.Context(), `SELECT current_user, current_schema()`).Scan(
		&runtimeRole,
		&schemaName,
	); err != nil {
		t.Fatalf("load restricted runtime identity: %v", err)
	}
	quotedRole := pgx.Identifier{runtimeRole}.Sanitize()
	quotedSchema := pgx.Identifier{schemaName}.Sanitize()
	for _, function := range []string{
		"provider_control_list_runnable_tenants(text,integer)",
		"provider_control_runtime_authority()",
	} {
		// #nosec G202 -- schema/role identifiers are read from the isolated test
		// connections and quoted; function signatures are fixed constants.
		statement := "GRANT EXECUTE ON FUNCTION " + quotedSchema + "." + function + " TO " + quotedRole
		if _, err := adminDatabase.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("grant bounded runtime function %s: %v", function, err)
		}
	}

	profiles, err := NewPostgresCatalogExecutionProfileResolver(runtimeDatabase)
	if err != nil {
		t.Fatalf("create restricted catalog resolver: %v", err)
	}
	admission, _ := newNativeAdmissionIntegrationServiceWithResolver(
		t,
		runtimeDatabase,
		profiles,
		staticAdmissionCapacityPolicy{grant: ownerLimitedCapacityGrant("owner-1", 100)},
	)
	request := nativeProvisionAdmissionFixture()
	request.Lease.Subject.ID = request.TenantID
	request.Lease.Metadata = map[string]string{runtimeOfferingMetadataKey: "monthly-runtime-standard"}
	if _, err := admission.AdmitProvision(t.Context(), request); err != nil {
		t.Fatalf("restricted native admission: %v", err)
	}

	ledger, err := NewPostgresLedger(runtimeDatabase, nil)
	if err != nil {
		t.Fatalf("NewPostgresLedger: %v", err)
	}
	page, err := ledger.ListRunnableTenants(t.Context(), "", 10)
	if err != nil {
		t.Fatalf("bounded tenant discovery: %v", err)
	}
	if len(page.TenantIDs) != 1 || page.TenantIDs[0] != request.TenantID || page.NextCursor != "" {
		t.Fatalf("bounded tenant page = %#v, want only %q", page, request.TenantID)
	}
	if _, err := runtimeDatabase.ExecContext(t.Context(), `
		SELECT tenant_id
		FROM provider_control_list_runnable_tenants('', NULL)
	`); err == nil {
		t.Fatal("NULL runnable-tenant limit bypassed the hard bound")
	}

	var rawOperationCount int
	if err := runtimeDatabase.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM provider_operations`).Scan(&rawOperationCount); err != nil {
		t.Fatalf("raw FORCE-RLS operation read: %v", err)
	}
	if rawOperationCount != 0 {
		t.Fatalf("unscoped runtime role saw %d raw provider operations", rawOperationCount)
	}
	if _, err := runtimeDatabase.ExecContext(t.Context(), `
		INSERT INTO provider_control_runnable_tenants (tenant_id, runnable_operation_count)
		VALUES ('injected-tenant', 1)
	`); err == nil {
		t.Fatal("runtime role injected a raw runnable-tenant directory row")
	}
	if _, err := runtimeDatabase.QueryContext(t.Context(), `
		SELECT tenant_id
		FROM provider_incident_list_tenant_ids()
	`); err == nil {
		t.Fatal("runtime role executed provider incident tenant enumerator")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("runtime incident tenant enumerator error = %v, want SQLSTATE 42501", err)
		}
	}

	for _, function := range []struct {
		name    string
		allowed bool
	}{
		{name: fmt.Sprintf("%s.provider_control_list_runnable_tenants(text,integer)", quotedSchema), allowed: true},
		{name: fmt.Sprintf("%s.provider_control_runtime_authority()", quotedSchema), allowed: true},
		{name: fmt.Sprintf("%s.provider_incident_list_tenant_ids()", quotedSchema), allowed: false},
		{name: fmt.Sprintf("%s.provider_execution_immutable_update()", quotedSchema), allowed: false},
		{name: fmt.Sprintf("%s.provider_execution_claim_credential_guard()", quotedSchema), allowed: false},
	} {
		var allowed bool
		if err := runtimeDatabase.QueryRowContext(t.Context(), `
			SELECT pg_catalog.has_function_privilege(current_user, $1, 'EXECUTE')
		`, function.name).Scan(&allowed); err != nil {
			t.Fatalf("inspect function privilege %s: %v", function.name, err)
		}
		if allowed != function.allowed {
			t.Fatalf("function %s executable = %v, want %v", function.name, allowed, function.allowed)
		}
	}
}

type manifestHashQueueExecutor struct {
	*queueExecutor
	manifestHash string
}

func (m manifestHashQueueExecutor) AtMostOnceProvisionCapability() AtMostOnceProvisionCapability {
	return AtMostOnceProvisionCapability{
		AdapterManifestHash:          m.manifestHash,
		PerHeadInvocationKey:         true,
		SideEffectFreePreparation:    true,
		PreparedRequestDigestBinding: true,
		ReadOnlyProvisionPolling:     true,
		ReadOnlyGeneralObservation:   true,
		ExactHandleDecommission:      true,
		ReadOnlyAbsencePolling:       true,
	}
}

type stubPreparedProvisionRequest struct {
	binding PreparedProvisionBinding
}

func (r stubPreparedProvisionRequest) ProvisionBinding() PreparedProvisionBinding {
	return r.binding
}

func (m manifestHashQueueExecutor) PrepareProvision(_ context.Context, inv AdapterInvocation) (PreparedProvisionRequest, error) {
	return stubPreparedProvisionRequest{
		binding: PreparedProvisionBinding{
			RequestDigest:         digest(inv.Key),
			CredentialVersionHash: inv.Request.Command.CustodyHash,
			ProviderScopeHash:     inv.Request.Command.ConnectionHash,
			CorrelationHash:       sha256Digest([]byte(inv.CorrelationID)),
			AdapterManifestHash:   m.manifestHash,
		},
	}, nil
}

func (m manifestHashQueueExecutor) DispatchPreparedProvision(_ context.Context, _ PreparedProvisionRequest, _ ExecutePermit) providerexecutor.ExecutionResult {
	return m.execute(ExecutionClaimSideEffecting)
}

func (m manifestHashQueueExecutor) PollProvisionPresence(_ context.Context, _ AdapterInvocation) providerexecutor.ExecutionResult {
	return m.execute(ExecutionClaimReadOnly)
}

func (m manifestHashQueueExecutor) ObserveReadOnly(_ context.Context, _ AdapterInvocation) providerexecutor.ExecutionResult {
	return m.execute(ExecutionClaimReadOnly)
}

func (m manifestHashQueueExecutor) DecommissionExactHandles(_ context.Context, _ AdapterInvocation) providerexecutor.ExecutionResult {
	return m.execute(ExecutionClaimSideEffecting)
}

func (m manifestHashQueueExecutor) PollDecommissionAbsence(_ context.Context, _ AdapterInvocation) providerexecutor.ExecutionResult {
	return m.execute(ExecutionClaimReadOnly)
}

type perOwnerLimitedCapacityPolicy struct {
	limit int
}

func (p perOwnerLimitedCapacityPolicy) ResolveCapacity(_ context.Context, req CapacityPolicyRequest) (CapacityGrant, error) {
	return ownerLimitedCapacityGrant(req.OwnerSubjectID, p.limit), nil
}

func TestSupportReplacementProviderControlLifecycleIntegration(t *testing.T) {
	if _, ok := AdapterDescriptorFor("ionos", "ionos-cloudapi-v6"); !ok {
		RegisterAdapterDescriptor(AdapterDescriptor{
			ProviderID:                "ionos",
			AdapterID:                 "ionos-cloudapi-v6",
			APIBase:                   "https://api.ionos.com/cloudapi/v6",
			CredentialEnvironment:     "IONOS_PROVIDER_BUNDLE",
			CredentialFields:          []string{"api_token", "contract_number", "token_expires_at", "ssh_private_key"},
			CatalogueImage:            "ubuntu:26.04",
			DefaultRegion:             "de/fra/2",
			BootstrapNetworkException: "ionos_public_lan_without_default_security_group",
			Capabilities:              []AdapterCapability{CapabilityPrivateImageImport},
			ManagedSSHPrivateKey:      func(time.Time) (string, error) { return "test-ssh-private-key", nil },
		})
	}
	if _, ok := AdapterDescriptorFor("ionos", "ionos-cloudapi-v6-memory-s"); !ok {
		RegisterAdapterDescriptor(AdapterDescriptor{
			ProviderID:                "ionos",
			AdapterID:                 "ionos-cloudapi-v6-memory-s",
			ExecutionOfferingID:       monthlyruntime.IONOSStandardMemoryExecutionOfferingID,
			APIBase:                   "https://api.ionos.com/cloudapi/v6",
			CredentialEnvironment:     "IONOS_PROVIDER_BUNDLE",
			CredentialFields:          []string{"api_token", "contract_number", "token_expires_at", "ssh_private_key"},
			CatalogueImage:            "ubuntu:26.04",
			DefaultRegion:             "de/fra/2",
			BootstrapNetworkException: "ionos_public_lan_without_default_security_group",
			Capabilities:              []AdapterCapability{CapabilityPrivateImageImport},
			ManagedSSHPrivateKey:      func(time.Time) (string, error) { return "test-ssh-private-key", nil },
		})
	}

	adminDB := openNativeAdmissionIntegrationDB(t)
	runtimeDB := openRestrictedNativeAdmissionRuntimeDB(t, adminDB)
	ctx := t.Context()

	ownerExec := func(tenantID, query string, args ...any) {
		t.Helper()
		tx, err := adminDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin owner tx (%s): %v", tenantID, err)
		}
		defer func() { _ = tx.Rollback() }()
		if tenantID != "" {
			if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, tenantContextKey, tenantID); err != nil {
				t.Fatalf("set owner tx tenant (%s): %v", tenantID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("owner sql (%s): %v", tenantID, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit owner tx (%s): %v", tenantID, err)
		}
	}

	// Activate a test catalog carrying both Basic Cube S and Memory Cube S profiles.
	ownerExec("", `
		INSERT INTO provider_catalog_versions (catalog_version, status)
		VALUES ('catalog-sr-lifecycle', 'draft');
		INSERT INTO provider_catalog_profiles (
			catalog_version, provider_id, adapter_id, credential_mode,
			runtime_profile_id, offering_id, can_pause, stop_effect,
			can_recreate, capability_snapshot, adapter_manifest_hash,
			provision_dispatch_mode
		)
		SELECT
			'catalog-sr-lifecycle', provider_id, adapter_id, credential_mode,
			runtime_profile_id, offering_id, can_pause, stop_effect,
			can_recreate, capability_snapshot, adapter_manifest_hash,
			provision_dispatch_mode
		FROM provider_catalog_profiles
		WHERE catalog_version = 'managed-vps-2026-11-16.1'
		  AND provider_id = 'ionos'
		  AND adapter_id IN ('ionos-cloudapi-v6', 'ionos-cloudapi-v6-memory-s');
		UPDATE provider_catalog_versions SET status = 'retired', retired_at = clock_timestamp() WHERE status = 'active';
		UPDATE provider_catalog_versions SET status = 'active', activated_at = clock_timestamp() WHERE catalog_version = 'catalog-sr-lifecycle';
	`)

	seedUserCredHandle := func(tenantID, subjectID string) {
		h := testCredentialHandle()
		h.SubjectKind = "user"
		h.SubjectID = subjectID
		h.CustodyRef = "custody://" + tenantID + "/provider-access"
		h.ConnectionRef = "provider-connection://" + tenantID + "/managed-compute"
		ch, err := credentialCustodyHash(tenantID, h)
		if err != nil {
			t.Fatalf("custody hash (%s): %v", tenantID, err)
		}
		connh, err := credentialConnectionHash(tenantID, h)
		if err != nil {
			t.Fatalf("connection hash (%s): %v", tenantID, err)
		}
		ownerExec(tenantID, `
			INSERT INTO provider_credential_handles (
				tenant_id, handle_id, handle_version, provider_id, credential_mode,
				subject_kind, subject_id, grant_id, credential_scope,
				custody_ref, connection_ref, custody_hash, connection_hash,
				valid_from, valid_until
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9,
				$10, $11, $12, $13, clock_timestamp() - interval '1 hour', clock_timestamp() + interval '24 hours'
			)
		`, tenantID, h.HandleID, h.HandleVersion, h.ProviderID, string(h.CredentialMode),
			h.SubjectKind, h.SubjectID, h.GrantID, h.Scope,
			h.CustodyRef, h.ConnectionRef, ch, connh)
	}
	seedUserCredHandle("tenant-1", "owner-1")

	profiles, err := NewPostgresCatalogExecutionProfileResolver(runtimeDB)
	if err != nil {
		t.Fatalf("NewPostgresCatalogExecutionProfileResolver: %v", err)
	}
	srcResources := []providerexecutor.ResourceBinding{
		{
			BindingID:     "server",
			Kind:          "ionos.server",
			NativeRef:     "https://api.ionos.com/cloudapi/v6/datacenters/dc-1/servers/ionos-vm-1",
			OwnershipHash: digest("ionos.server\x00https://api.ionos.com/cloudapi/v6/datacenters/dc-1/servers/ionos-vm-1"),
			Disposition:   providerexecutor.DispositionDelete,
			Observation:   providerexecutor.ObservationPresent,
			Cleanup:       providerexecutor.CleanupRequired,
		},
		{
			BindingID:     "public-ip",
			Kind:          "ionos.public_ip",
			NativeRef:     "https://85.215.10.20",
			OwnershipHash: digest("ionos.public_ip\x00https://85.215.10.20"),
			Disposition:   providerexecutor.DispositionDelete,
			Observation:   providerexecutor.ObservationPresent,
			Cleanup:       providerexecutor.CleanupRequired,
		},
	}
	srcQueue := &queueExecutor{
		results: []providerexecutor.ExecutionResult{
			{Status: providerexecutor.StatusPending, Phase: providerexecutor.PhaseResourcesBound, Resources: srcResources},
			{Status: providerexecutor.StatusSucceeded, Phase: providerexecutor.PhasePresent, Resources: srcResources},
		},
	}
	registry := NewRegistry()
	if err := registry.RegisterAtMostOnceProvision("ionos-cloudapi-v6", manifestHashQueueExecutor{
		queueExecutor: srcQueue,
		manifestHash:  "sha256:640fcf4852e807ed28fa571bdad0ff2b7a21d74a4be975f2729ca5c6e3aef8ad",
	}); err != nil {
		t.Fatalf("register ionos-cloudapi-v6: %v", err)
	}
	if err := registry.RegisterAtMostOnceProvision("ionos-cloudapi-v6-memory-s", manifestHashQueueExecutor{
		queueExecutor: &queueExecutor{},
		manifestHash:  "sha256:6167cd0c480813ea24c3730bde830958fe0ff2042f572a4e89ba53606d4e5fed",
	}); err != nil {
		t.Fatalf("register ionos-cloudapi-v6-memory-s: %v", err)
	}
	ledger, err := NewPostgresLedger(runtimeDB, nil)
	if err != nil {
		t.Fatalf("NewPostgresLedger: %v", err)
	}
	coordinator, err := NewCoordinator(CoordinatorConfig{
		Registry: registry, Profiles: profiles, Ledger: ledger, ActivationGate: AllowGate{},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	admission, err := NewNativeAdmission(NativeAdmissionConfig{
		Database: runtimeDB, Coordinator: coordinator, ActivationGate: AllowGate{},
		ProviderCreates: AllowProviderCreate{},
		CapacityPolicy:  perOwnerLimitedCapacityPolicy{limit: 1},
	})
	if err != nil {
		t.Fatalf("NewNativeAdmission: %v", err)
	}

	// 1. Seed a Basic Cube S source generation (ordinal 1) via real AdmitProvision.
	srcReq := nativeProvisionAdmissionFixture()
	srcReq.RuntimeSlotKey = "foundation"
	srcReq.RuntimeSlotGeneration = 1
	srcReq.Lease.Subject = vmlease.Subject{Kind: vmlease.SubjectUser, ID: "owner-1", OrgID: "tenant-1"}
	srcReq.Lease.Resource.ProviderID = "ionos"
	srcReq.Lease.Resource.Region = "de/fra/2"
	srcReq.Lease.Metadata = map[string]string{
		runtimeOfferingMetadataKey:                string(serverruntime.RuntimeOfferingStandard),
		monthlyruntime.MetadataKeyProviderPackage: "Basic Cube S",
		"stack_id":           "stack-native-1",
		"stack_name":         "Cloud Stack One",
		"stackkit":           "cloud-kit",
		"node_role":          "foundation",
		"requested_services": "coolify",
		"provider_region":    "de/fra/2",
	}
	srcReq.DesiredSpec = json.RawMessage(`{"schema_version":"homelab/provider-desired-spec/v2","provider_id":"ionos","offering_id":"monthly-runtime-standard","provider_package":"Basic Cube S","region":"de/fra/2","stack_id":"stack-native-1","stack_name":"Cloud Stack One","runtime_slot_key":"foundation"}`)
	srcResult, err := admission.AdmitProvision(ctx, srcReq)
	if err != nil {
		t.Fatalf("AdmitProvision Basic Cube S source: %v", err)
	}
	// P2-3: While provision is pending/not yet active, AuthorizeSupportReplacement MUST fail closed.
	if _, err := admission.AuthorizeSupportReplacement(ctx, AuthorizeSupportReplacementRequest{
		TenantID: "tenant-1", ReplacementID: "sr-premature", OwnerSubjectID: "owner-1",
		StackID: "stack-native-1", SourceLeaseID: srcResult.LeaseID,
		CaseRef: "CASE-PRE", Reason: "premature", AuthorizedBy: "staff@kombify.io",
		EvidenceRef: "ev://pre", IdempotencyKey: "auth-pre", NoChargeConfirmed: true,
	}); !errors.Is(err, ErrNativeAdmissionConflict) {
		t.Fatalf("expected non-active/non-succeeded source to be rejected, got %v", err)
	}
	for step := 1; step <= 3; step++ {
		if _, _, advErr := coordinator.Advance(ctx, "tenant-1", srcResult.Operation.Command.OperationID); advErr != nil {
			t.Fatalf("coordinator.Advance source step %d: %v", step, advErr)
		}
	}
	ownerExec("tenant-1", `
		UPDATE servers SET lifecycle_state = 'active', connection_state = 'connected'
		WHERE tenant_id = 'tenant-1' AND id = $1
	`, srcResult.RuntimeServerID)

	// Seed Basic Cube M and empty-package sources on tenant-m to verify P2-3 strict eligibility.
	ownerExec("tenant-m", `
		INSERT INTO techstack_tenants (id, display_name, kind, status) VALUES ('tenant-m', 'Tenant M', 'saas', 'active');
		INSERT INTO stacks (id, tenant_id, owner_subject_id, name, status) VALUES ('stack-m', 'tenant-m', 'owner-m', 'M Stack', 'active');
	`)
	seedUserCredHandle("tenant-m", "owner-m")
	mReq := srcReq
	mReq.TenantID = "tenant-m"
	mReq.OwnerSubjectID = "owner-m"
	mReq.RuntimeSlotID = DeriveManagedRuntimeSlotID("tenant-m", "stack-m", "foundation")
	mReq.RuntimeServerID = "srv-m-1"
	mReq.IdempotencyKey = "idem-m-1"
	mReq.Lease.ID = "lease-m-1"
	mReq.Lease.Subject = vmlease.Subject{Kind: vmlease.SubjectUser, ID: "owner-m", OrgID: "tenant-m"}
	mReq.Lease.Metadata = map[string]string{
		runtimeOfferingMetadataKey:                string(serverruntime.RuntimeOfferingStandard),
		monthlyruntime.MetadataKeyProviderPackage: "Basic Cube M",
		"stack_id": "stack-m",
	}
	mReq.Server.StackID = "stack-m"
	mReq.DesiredSpecRef = "desired-spec://homelab/leases/lease-m-1/revisions/1"
	if _, err := admission.AdmitProvision(ctx, mReq); err != nil {
		t.Fatalf("AdmitProvision Basic Cube M: %v", err)
	}
	if _, err := admission.AuthorizeSupportReplacement(ctx, AuthorizeSupportReplacementRequest{
		TenantID: "tenant-m", StackID: "stack-m", SourceLeaseID: "lease-m-1",
		CaseRef: "CASE-M", Reason: "should reject Basic Cube M", AuthorizedBy: "staff@kombify.io",
		EvidenceRef: "ev://m", IdempotencyKey: "auth-m", NoChargeConfirmed: true,
	}); !errors.Is(err, ErrNativeAdmissionConflict) {
		t.Fatalf("expected Basic Cube M to be rejected with ErrNativeAdmissionConflict, got %v", err)
	}

	// 2. Authorize (P2-11 ReplacementID in digest) & Accept (P2-1 owner+acceptedBy match).
	authRec, err := admission.AuthorizeSupportReplacement(ctx, AuthorizeSupportReplacementRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-1", OwnerSubjectID: "owner-1",
		StackID: "stack-native-1", SourceLeaseID: srcResult.LeaseID,
		CaseRef: "CASE-100", Reason: "Free Memory Cube S upgrade",
		AuthorizedBy: "staff@kombify.io", EvidenceRef: "ticket://CASE-100",
		IdempotencyKey: "auth-sr-1", NoChargeConfirmed: true,
	})
	if err != nil {
		t.Fatalf("AuthorizeSupportReplacement failed: %v", err)
	}
	if authRec.SourceExecutionProfile != "ionos-basic-cube-standard" {
		t.Fatalf("SourceExecutionProfile = %q, want ionos-basic-cube-standard", authRec.SourceExecutionProfile)
	}
	if _, err := admission.AuthorizeSupportReplacement(ctx, AuthorizeSupportReplacementRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-other", OwnerSubjectID: "owner-1",
		StackID: "stack-native-1", SourceLeaseID: srcResult.LeaseID,
		CaseRef: "CASE-100", Reason: "Free Memory Cube S upgrade",
		AuthorizedBy: "staff@kombify.io", EvidenceRef: "ticket://CASE-100",
		IdempotencyKey: "auth-sr-1", NoChargeConfirmed: true,
	}); !errors.Is(err, ErrNativeAdmissionConflict) {
		t.Fatalf("expected replay with different replacement_id to conflict, got %v", err)
	}
	if _, err := admission.AcceptSupportReplacement(ctx, AcceptSupportReplacementRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-1", AcceptedBy: "owner-1",
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected missing ownerSubjectId accept error, got %v", err)
	}
	if _, err := admission.AcceptSupportReplacement(ctx, AcceptSupportReplacementRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-1", OwnerSubjectID: "owner-1", AcceptedBy: "staff@kombify.io",
	}); !errors.Is(err, ErrNativeAdmissionConflict) {
		t.Fatalf("expected mismatched acceptedBy conflict, got %v", err)
	}
	if _, err := admission.AcceptSupportReplacement(ctx, AcceptSupportReplacementRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-1", OwnerSubjectID: "wrong-owner", AcceptedBy: "wrong-owner",
	}); !errors.Is(err, ErrNativeAdmissionConflict) {
		t.Fatalf("expected cross-owner accept conflict, got %v", err)
	}
	acceptedRec, err := admission.AcceptSupportReplacement(ctx, AcceptSupportReplacementRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-1", OwnerSubjectID: "owner-1", AcceptedBy: "owner-1",
	})
	if err != nil || acceptedRec.Phase != SupportReplacementPhaseAccepted {
		t.Fatalf("AcceptSupportReplacement failed: %v rec=%+v", err, acceptedRec)
	}

	// 3. AdmitSupportReplacementProvision + idempotent replay + P2-4 slot resolution + capacity enforcement.
	provReq := SupportReplacementProvisionRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-1", IdempotencyKey: "sr-admit:sr-e2e-1",
	}
	provRes, srAfterAdmit, err := admission.AdmitSupportReplacementProvision(ctx, provReq)
	if err != nil {
		t.Fatalf("AdmitSupportReplacementProvision failed: %v", err)
	}
	if provRes.RuntimeSlotGeneration != 2 || srAfterAdmit.Phase != SupportReplacementPhaseDestinationAdmitted ||
		srAfterAdmit.DestinationGenerationOrdinal == nil || *srAfterAdmit.DestinationGenerationOrdinal != 2 {
		t.Fatalf("unexpected admission result: %+v sr=%+v", provRes, srAfterAdmit)
	}
	resolved, err := admission.ResolveManagedRuntimeSlotGeneration(ctx, "tenant-1", "stack-native-1", "foundation")
	if err != nil || resolved.GenerationOrdinal != 2 || !resolved.ExistingUnreleased {
		t.Fatalf("ResolveManagedRuntimeSlotGeneration with 2 active generations = %+v err=%v", resolved, err)
	}
	replayRes, _, err := admission.AdmitSupportReplacementProvision(ctx, provReq)
	if err != nil || replayRes.LeaseID != provRes.LeaseID || replayRes.RuntimeServerID != provRes.RuntimeServerID {
		t.Fatalf("AdmitSupportReplacementProvision replay failed: err=%v replay=%+v orig=%+v", err, replayRes, provRes)
	}

	extraReq := nativeProvisionAdmissionFixture()
	extraReq.RuntimeSlotKey = "secondary"
	extraReq.RuntimeSlotID = DeriveManagedRuntimeSlotID("tenant-1", "stack-native-1", "secondary")
	extraReq.RuntimeServerID = "srv-extra-1"
	extraReq.Lease.ID = "lease-extra-1"
	extraReq.Lease.Subject = vmlease.Subject{Kind: vmlease.SubjectUser, ID: "owner-1", OrgID: "tenant-1"}
	extraReq.IdempotencyKey = "idem-extra-1"
	extraReq.DesiredSpecRef = "desired-spec://homelab/leases/lease-extra-1/revisions/1"
	if _, err := admission.AdmitProvision(ctx, extraReq); err == nil {
		t.Fatal("expected 2nd unrelated AdmitProvision to be rejected on 1-slot owner")
	}

	seedAbsenceAndRelease := func(tenantID, leaseID, genID, serverID, tag string) {
		relOpID := "decom-" + tag
		rcpt1Digest := digest("rcpt1-" + tag)
		rcpt2Digest := digest("rcpt2-" + tag)
		cmdDigest := digest("cmd-" + tag)
		evDigest := digest("ev-" + tag)
		attDigest := digest("att-" + tag)
		subjDigest := digest("subj-" + tag)
		natDigest := digest("nat-" + tag)
		ownerExec(tenantID, `
			UPDATE servers
			SET lifecycle_state = 'decommissioned', desired_state = 'absent', decommissioned_at = clock_timestamp()
			WHERE tenant_id = $1 AND id = $2
		`, tenantID, serverID)
		ownerExec(tenantID, `
			INSERT INTO provider_operations (
				tenant_id, operation_id, lease_id, operation, idempotency_key, adapter_id,
				command_digest, command_json, ledger_revision, desired_spec_revision,
				status, phase, head_sequence, head_receipt_digest, requested_at, created_at, updated_at,
				provision_dispatch_mode
			)
			SELECT
				$1, $2, $3, 'decommission', 'idem-' || $2, p.adapter_id,
				$4, p.command_json, 1, NULL,
				'succeeded', 'absent', 2, $5, now(), now(), now(),
				p.provision_dispatch_mode
			FROM provider_operations AS p
			WHERE p.tenant_id = $1 AND p.lease_id = $3 AND p.operation = 'provision'
			ORDER BY p.created_at DESC
			LIMIT 1
		`, tenantID, relOpID, leaseID, cmdDigest, rcpt2Digest)
		ownerExec(tenantID, `
			INSERT INTO provider_operation_receipts (
				tenant_id, operation_id, sequence, previous_receipt_digest, receipt_digest, status, phase, receipt_json, issued_at
			) VALUES (
				$1, $2, 1, NULL, $3, 'pending', 'requested', '{}'::jsonb, clock_timestamp()
			), (
				$1, $2, 2, $3, $4, 'succeeded', 'absent', '{}'::jsonb, clock_timestamp()
			)
		`, tenantID, relOpID, rcpt1Digest, rcpt2Digest)
		ownerExec(tenantID, `
			INSERT INTO provider_absence_observations (
				tenant_id, operation_id, binding_id, provider_id,
				evidence_ref, evidence_digest, attestation_ref, attestation_digest,
				observation, source, subject_hash, native_ref_hash, collected_at, document_json
			) VALUES (
				$1, $2, 'bind-' || $2, 'ionos',
				'provider-evidence://ionos/' || $2, $3, 'provider-attestation://ionos/' || $2, $4,
				'absent', 'provider_api', $5, $6, clock_timestamp(), '{}'::jsonb
			)
		`, tenantID, relOpID, evDigest, attDigest, subjDigest, natDigest)
		ownerExec(tenantID, `
			INSERT INTO managed_runtime_capacity_release_facts (
				tenant_id, lease_id, resource_generation_id, server_id, server_generation,
				release_operation_id, release_authority, receipt_sequence, receipt_digest, released_at
			) VALUES (
				$1, $2, $3::uuid, $4, 1, $5, 'provider_absence', 2, $6, clock_timestamp()
			)
		`, tenantID, leaseID, genID, serverID, relOpID, rcpt2Digest)
	}

	// 4. CLI Rollback guard: refuse before absence+release exist, allow once destination is absent & released.
	if _, err := admission.RollbackSupportReplacementFromCLI(ctx, "tenant-1", "sr-e2e-1", "abort"); !errors.Is(err, ErrNativeAdmissionConflict) {
		t.Fatalf("expected CLI rollback refusal before destination absence/release, got %v", err)
	}
	// The migration 205 fence refuses destination decommission evidence while the
	// replacement is destination_admitted; a pre-write rollback first moves to
	// rolling_back, which admits the destination teardown.
	if _, err := admission.TransitionSupportReplacement(ctx, SupportReplacementTransitionRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-1", Phase: SupportReplacementPhaseRollingBack,
		RollbackReason: "pre-write rollback",
	}); err != nil {
		t.Fatalf("rolling_back before destination teardown: %v", err)
	}
	seedAbsenceAndRelease("tenant-1", provRes.LeaseID, provRes.ResourceGenerationID, provRes.RuntimeServerID, "dest-2")
	validatedRB, err := admission.ValidateSupportReplacementCLIRollback(ctx, "tenant-1", "sr-e2e-1", "pre-write rollback")
	if err != nil || validatedRB.Phase != SupportReplacementPhaseRollingBack {
		t.Fatalf("ValidateSupportReplacementCLIRollback failed without mutating phase: %v rec=%+v", err, validatedRB)
	}
	rbRec, err := admission.RollbackSupportReplacementFromCLI(ctx, "tenant-1", "sr-e2e-1", "pre-write rollback")
	if err != nil || rbRec.Phase != SupportReplacementPhaseRolledBack {
		t.Fatalf("RollbackSupportReplacementFromCLI failed: %v rec=%+v", err, rbRec)
	}

	// 5. Retry support replacement on active Basic Cube S ordinal 1 -> ordinal 3.
	if _, err := admission.AuthorizeSupportReplacement(ctx, AuthorizeSupportReplacementRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-2", OwnerSubjectID: "owner-1",
		StackID: "stack-native-1", SourceLeaseID: srcResult.LeaseID,
		CaseRef: "CASE-101", Reason: "Retry Memory Cube S upgrade",
		AuthorizedBy: "staff@kombify.io", EvidenceRef: "ticket://CASE-101",
		IdempotencyKey: "auth-sr-2", NoChargeConfirmed: true,
	}); err != nil {
		t.Fatalf("AuthorizeSupportReplacement retry failed: %v", err)
	}
	if _, err := admission.AcceptSupportReplacement(ctx, AcceptSupportReplacementRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-2", OwnerSubjectID: "owner-1", AcceptedBy: "owner-1",
	}); err != nil {
		t.Fatalf("AcceptSupportReplacement retry failed: %v", err)
	}
	retryProvRes, retrySR, err := admission.AdmitSupportReplacementProvision(ctx, SupportReplacementProvisionRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-2", IdempotencyKey: "sr-admit:sr-e2e-2",
	})
	if err != nil {
		t.Fatalf("AdmitSupportReplacementProvision retry failed: %v", err)
	}
	if retryProvRes.RuntimeSlotGeneration != 3 || retrySR.DestinationGenerationOrdinal == nil || *retrySR.DestinationGenerationOrdinal != 3 {
		t.Fatalf("expected ordinal 3 on retry, got %+v sr=%+v", retryProvRes, retrySR)
	}

	// 6. Test P1-8 ReconcileSupportReplacementConflict invariants on fenced_conflict.
	writesTrue := true
	if _, err := admission.TransitionSupportReplacement(ctx, SupportReplacementTransitionRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-2", Phase: SupportReplacementPhaseStaging,
	}); err != nil {
		t.Fatalf("staging: %v", err)
	}
	if _, err := admission.TransitionSupportReplacement(ctx, SupportReplacementTransitionRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-2", Phase: SupportReplacementPhaseCutoverInProgress,
		DestinationWritesObserved: &writesTrue,
	}); err != nil {
		t.Fatalf("cutover_in_progress: %v", err)
	}
	if _, err := admission.TransitionSupportReplacement(ctx, SupportReplacementTransitionRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-2", Phase: SupportReplacementPhaseFencedConflict,
		FencedConflictReason: "ack lost",
	}); err != nil {
		t.Fatalf("fenced_conflict: %v", err)
	}
	// Cannot reconcile to rolling_back once destination_writes_observed is true.
	if _, err := admission.ReconcileSupportReplacementConflict(ctx, "tenant-1", "sr-e2e-2", SupportReplacementPhaseRollingBack, "invalid"); !errors.Is(err, ErrNativeAdmissionConflict) {
		t.Fatalf("expected reconcile rolling_back refusal after destination writes, got %v", err)
	}
	// Cannot reconcile to completed before source is absent & released.
	if _, err := admission.ValidateSupportReplacementConflictReconcile(ctx, "tenant-1", "sr-e2e-2", SupportReplacementPhaseCompleted, "try early"); !errors.Is(err, ErrNativeAdmissionConflict) {
		t.Fatalf("expected validate reconcile completed refusal before source absence/release, got %v", err)
	}
	if _, err := admission.ReconcileSupportReplacementConflict(ctx, "tenant-1", "sr-e2e-2", SupportReplacementPhaseCompleted, "try early"); !errors.Is(err, ErrNativeAdmissionConflict) {
		t.Fatalf("expected reconcile completed refusal before source absence/release, got %v", err)
	}
	// Can reconcile forward to cutover_in_progress when destination server is active!
	ownerExec("tenant-1", `
		UPDATE servers SET lifecycle_state = 'active', connection_state = 'connected'
		WHERE tenant_id = 'tenant-1' AND id = $1
	`, retryProvRes.RuntimeServerID)
	resumedCutover, err := admission.ReconcileSupportReplacementConflict(ctx, "tenant-1", "sr-e2e-2", SupportReplacementPhaseCutoverInProgress, "resume forward cutover")
	if err != nil || resumedCutover.Phase != SupportReplacementPhaseCutoverInProgress {
		t.Fatalf("ReconcileSupportReplacementConflict -> cutover_in_progress failed: %v rec=%+v", err, resumedCutover)
	}
	// The resumed workflow verifies the cutover and starts the source teardown;
	// the fence admits the source decommission only from there. A teardown
	// conflict then re-enters fenced_conflict and is reconciled to completed.
	cutoverAt := time.Now().UTC()
	if _, err := admission.TransitionSupportReplacement(ctx, SupportReplacementTransitionRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-2", Phase: SupportReplacementPhaseCutoverVerified,
		CutoverVerifiedAt: &cutoverAt,
	}); err != nil {
		t.Fatalf("cutover_verified after resumed cutover: %v", err)
	}
	if _, err := admission.TransitionSupportReplacement(ctx, SupportReplacementTransitionRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-2", Phase: SupportReplacementPhaseSourceDecommissioning,
		SourceTeardownOperationID: "decom-src-1",
	}); err != nil {
		t.Fatalf("source_decommissioning: %v", err)
	}
	seedAbsenceAndRelease("tenant-1", srcResult.LeaseID, srcResult.ResourceGenerationID, srcResult.RuntimeServerID, "src-1")
	if _, err := admission.TransitionSupportReplacement(ctx, SupportReplacementTransitionRequest{
		TenantID: "tenant-1", ReplacementID: "sr-e2e-2", Phase: SupportReplacementPhaseFencedConflict,
		FencedConflictReason: "source teardown conflict",
	}); err != nil {
		t.Fatalf("re-enter fenced_conflict: %v", err)
	}
	if _, err := admission.ValidateSupportReplacementConflictReconcile(ctx, "tenant-1", "sr-e2e-2", SupportReplacementPhaseRollingBack, "invalid"); !errors.Is(err, ErrNativeAdmissionConflict) {
		t.Fatalf("expected validate reconcile rolling_back refusal after destination writes, got %v", err)
	}
	validatedDone, err := admission.ValidateSupportReplacementConflictReconcile(ctx, "tenant-1", "sr-e2e-2", SupportReplacementPhaseCompleted, "verified source gone and dest active")
	if err != nil || validatedDone.Phase != SupportReplacementPhaseFencedConflict {
		t.Fatalf("ValidateSupportReplacementConflictReconcile -> completed should leave phase fenced_conflict before commit: %v rec=%+v", err, validatedDone)
	}
	reconciledDone, err := admission.ReconcileSupportReplacementConflict(ctx, "tenant-1", "sr-e2e-2", SupportReplacementPhaseCompleted, "verified source gone and dest active")
	if err != nil || reconciledDone.Phase != SupportReplacementPhaseCompleted || reconciledDone.CutoverVerifiedAt == nil {
		t.Fatalf("ReconcileSupportReplacementConflict -> completed failed: %v rec=%+v", err, reconciledDone)
	}
	_ = sql.ErrNoRows
}
