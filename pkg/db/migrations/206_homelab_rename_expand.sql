-- Phase 1 Expand for Homelab rename (platform-2jlnx7.7).
--
-- Widens CHECK constraints, provider-control / decommission-wait / backup /
-- household SQL functions, and session_user RLS policies so both legacy
-- techstack* spellings (live homelab-v0.52.3/0.52.4 binaries) and forward
-- homelab* spellings are accepted concurrently. Adds the homelab_archive schema
-- and kombify_ctx.role_class registrations without creating login/nologin roles
-- (so NOCREATEROLE native owners and forward-batch tests continue to succeed),
-- creating views, granting new privileges to techstack_application /
-- techstack_backup_worker / techstack_provider_control_runtime, or adding
-- pg_auth_members memberships for login roles. Note: invoker trigger functions
-- (2d, 2g, 2h, 2j) also pin search_path = pg_catalog, public, pg_temp.
SET LOCAL lock_timeout = '5s';

-- ---------------------------------------------------------------------------
-- 1. Widen CHECK constraints
-- ---------------------------------------------------------------------------

ALTER TABLE user_onboarding_state DROP CONSTRAINT IF EXISTS user_onboarding_state_product_check;
ALTER TABLE user_onboarding_state ADD CONSTRAINT user_onboarding_state_product_check
    CHECK (product IN ('techstack', 'homelab'));

ALTER TABLE services DROP CONSTRAINT IF EXISTS services_source_check;
ALTER TABLE services ADD CONSTRAINT services_source_check
    CHECK (source IN (
        'observed',
        'stackkits-inventory',
        'stackkit_outputs',
        'techstack-registry',
        'homelab-registry',
        'legacy-registry-backfill'
    ));

ALTER TABLE runtime_lease_execution_authorities
    DROP CONSTRAINT IF EXISTS runtime_lease_execution_authorities_execution_authority_check;
ALTER TABLE runtime_lease_execution_authorities
    ADD CONSTRAINT runtime_lease_execution_authorities_execution_authority_check
    CHECK (execution_authority IN ('legacy_simulate', 'techstack_provider_control', 'homelab_provider_control'));

ALTER TABLE runtime_lease_execution_authorities
    DROP CONSTRAINT IF EXISTS runtime_lease_execution_authorities_check;
ALTER TABLE runtime_lease_execution_authorities
    ADD CONSTRAINT runtime_lease_execution_authorities_check
    CHECK (
        (
            execution_authority = 'legacy_simulate'
            AND NULLIF(BTRIM(evidence_resource_generation_id), '') IS NOT NULL
            AND jsonb_typeof(evidence_json) = 'object'
        )
        OR
        (
            execution_authority IN ('techstack_provider_control', 'homelab_provider_control')
            AND evidence_resource_generation_id IS NULL
            AND evidence_json IS NULL
        )
    );

-- ---------------------------------------------------------------------------
-- 2. Widen provider-control, decommission-wait, backup, and household routines
-- ---------------------------------------------------------------------------

-- 2a. provider_provision_dispatch_guard_validate_insert (029 + 035 posture)
CREATE OR REPLACE FUNCTION provider_provision_dispatch_guard_validate_insert()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    operation_kind text;
    operation_mode text;
    operation_lease_id text;
    operation_schema_version text;
    operation_execution_authority text;
    command_lease_revision text;
    command_server_id text;
    command_resource_generation_id text;
    command_capability_snapshot_hash text;
    command_execution_profile_hash text;
    command_custody_hash text;
    command_connection_hash text;
    profile_capability_snapshot_hash text;
    profile_execution_profile_hash text;
    profile_adapter_manifest_hash text;
    receipt_status text;
    receipt_phase text;
    receipt_resources jsonb;
    live_lease_revision bigint;
    live_server_id text;
    live_resource_generation_id uuid;
    bound_claim_token_digest text := CASE
        WHEN nullif(current_setting('app.provider_execution_claim_token', true), '') IS NULL THEN NULL
        ELSE encode(
            sha256(convert_to(current_setting('app.provider_execution_claim_token', true), 'UTF8')),
            'hex'
        )
    END;
    bound_claim_owner text := nullif(
        current_setting('app.provider_execution_claim_owner', true),
        ''
    );
BEGIN
    IF NEW.guard_origin IS DISTINCT FROM 'first_claim' THEN
        RAISE EXCEPTION 'runtime dispatch guards must originate from the first claim'
            USING ERRCODE = '55000';
    END IF;

    PERFORM pg_advisory_xact_lock(
        hashtext(NEW.tenant_id),
        hashtext(NEW.lease_id || ':' || NEW.resource_generation_id::text)
    );

    SELECT
        operation.operation,
        operation.provision_dispatch_mode,
        operation.lease_id,
        operation.command_json->>'schema_version',
        operation.command_json->>'execution_authority',
        operation.command_json #>> '{command,lease_revision}',
        operation.command_json #>> '{command,runtime_server_id}',
        operation.command_json #>> '{command,resource_generation_id}',
        operation.command_json #>> '{command,capability_snapshot_hash}',
        operation.command_json #>> '{command,execution_profile_hash}',
        operation.command_json #>> '{command,custody_hash}',
        operation.command_json #>> '{command,connection_hash}',
        operation.command_json #>> '{execution_profile,capability_snapshot_hash}',
        operation.command_json #>> '{execution_profile,execution_profile_hash}',
        operation.command_json #>> '{execution_profile,adapter_manifest_hash}',
        receipt.status,
        receipt.phase,
        receipt.receipt_json->'resources'
    INTO
        operation_kind,
        operation_mode,
        operation_lease_id,
        operation_schema_version,
        operation_execution_authority,
        command_lease_revision,
        command_server_id,
        command_resource_generation_id,
        command_capability_snapshot_hash,
        command_execution_profile_hash,
        command_custody_hash,
        command_connection_hash,
        profile_capability_snapshot_hash,
        profile_execution_profile_hash,
        profile_adapter_manifest_hash,
        receipt_status,
        receipt_phase,
        receipt_resources
    FROM provider_operations AS operation
    JOIN provider_operation_receipts AS receipt
      ON receipt.tenant_id = operation.tenant_id
     AND receipt.operation_id = operation.operation_id
     AND receipt.sequence = operation.head_sequence
     AND receipt.receipt_digest = operation.head_receipt_digest
    WHERE operation.tenant_id = NEW.tenant_id
      AND operation.operation_id = NEW.operation_id
      AND operation.head_sequence = NEW.head_sequence
      AND operation.head_receipt_digest = NEW.head_receipt_digest
    FOR SHARE OF operation, receipt;

    IF NOT FOUND
       OR operation_kind IS DISTINCT FROM 'provision'
       OR operation_mode IS DISTINCT FROM NEW.dispatch_mode
       OR operation_mode NOT IN (
            'native_idempotency',
            'provider_correlation',
            'at_most_once_dispatch_manual_reconcile'
       )
       OR operation_schema_version IS NULL
       OR operation_schema_version NOT IN (
            'techstack.provider-control-operation/v1',
            'homelab.provider-control-operation/v1'
       )
       OR operation_execution_authority IS NULL
       OR operation_execution_authority NOT IN (
            'techstack_provider_control',
            'homelab_provider_control'
       )
       OR receipt_status IS DISTINCT FROM 'pending'
       OR receipt_phase IS DISTINCT FROM 'accepted' THEN
        RAISE EXCEPTION 'dispatch custody must bind the current native provision accepted head'
            USING ERRCODE = '55000';
    END IF;

    SELECT
        runtime_lease.lease_revision,
        runtime_lease.server_id,
        runtime_lease.resource_generation_id
    INTO
        live_lease_revision,
        live_server_id,
        live_resource_generation_id
    FROM techstack_vm_leases AS runtime_lease
    WHERE runtime_lease.tenant_id = NEW.tenant_id
      AND runtime_lease.id = NEW.lease_id
    FOR SHARE OF runtime_lease;
    IF NOT FOUND
       OR live_lease_revision IS DISTINCT FROM NEW.lease_revision
       OR live_server_id IS DISTINCT FROM NEW.server_id
       OR live_resource_generation_id IS DISTINCT FROM NEW.resource_generation_id THEN
        RAISE EXCEPTION 'dispatch custody does not match the live typed runtime lease'
            USING ERRCODE = '55000';
    END IF;

    PERFORM 1
    FROM provider_operation_execution_claims AS prior_claim
    JOIN provider_operations AS prior_operation
      ON prior_operation.tenant_id = prior_claim.tenant_id
     AND prior_operation.operation_id = prior_claim.operation_id
    WHERE prior_operation.tenant_id = NEW.tenant_id
      AND prior_operation.lease_id = NEW.lease_id
      AND prior_operation.operation = 'provision'
      AND (prior_operation.command_json #>> '{command,resource_generation_id}')::uuid = NEW.resource_generation_id
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'resource generation already has provision execution custody'
            USING ERRCODE = '55000';
    END IF;
    IF receipt_resources IS NOT NULL THEN
        IF jsonb_typeof(receipt_resources) IS DISTINCT FROM 'array' THEN
            RAISE EXCEPTION 'dispatch guard requires a handle-free provision accepted receipt'
                USING ERRCODE = '55000';
        END IF;
        IF jsonb_array_length(receipt_resources) <> 0 THEN
            RAISE EXCEPTION 'dispatch guard requires a handle-free provision accepted receipt'
                USING ERRCODE = '55000';
        END IF;
    END IF;

    IF operation_lease_id IS DISTINCT FROM NEW.lease_id
       OR command_lease_revision IS DISTINCT FROM NEW.lease_revision::text
       OR command_server_id IS DISTINCT FROM NEW.server_id
       OR command_resource_generation_id IS DISTINCT FROM NEW.resource_generation_id::text
       OR command_capability_snapshot_hash IS DISTINCT FROM NEW.capability_snapshot_hash
       OR profile_capability_snapshot_hash IS DISTINCT FROM NEW.capability_snapshot_hash
       OR command_execution_profile_hash IS DISTINCT FROM NEW.execution_profile_hash
       OR profile_execution_profile_hash IS DISTINCT FROM NEW.execution_profile_hash
       OR profile_adapter_manifest_hash IS DISTINCT FROM NEW.adapter_manifest_hash THEN
        RAISE EXCEPTION 'dispatch guard lease, UUID, or execution-profile projection mismatch'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.dispatch_mode = 'at_most_once_dispatch_manual_reconcile' THEN
        IF command_custody_hash IS DISTINCT FROM NEW.credential_version_hash
           OR command_connection_hash IS DISTINCT FROM NEW.provider_scope_hash THEN
            RAISE EXCEPTION 'AMO prepared request custody or provider scope mismatch'
                USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.prepared_request_digest IS NOT NULL
       OR NEW.credential_version_hash IS NOT NULL
       OR NEW.provider_scope_hash IS NOT NULL
       OR NEW.correlation_hash IS NOT NULL THEN
        RAISE EXCEPTION 'crash-recoverable provision custody cannot carry AMO preparation fields'
            USING ERRCODE = '23514';
    END IF;

    IF bound_claim_token_digest IS NULL
       OR bound_claim_owner IS NULL
       OR NEW.first_claim_token_digest IS DISTINCT FROM bound_claim_token_digest
       OR NEW.first_claim_owner IS DISTINCT FROM bound_claim_owner THEN
        RAISE EXCEPTION 'dispatch guard is not bound to the transaction claim capability'
            USING ERRCODE = '55000';
    END IF;

    NEW.guarded_at := clock_timestamp();
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION provider_provision_dispatch_guard_validate_insert() FROM PUBLIC;

-- 2b. provider_operation_runtime_generation_guard (035)
CREATE OR REPLACE FUNCTION provider_operation_runtime_generation_guard()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    envelope_version text;
    execution_authority text;
    pinned_server_generation bigint;
    command_server_id text;
    command_provider_id text;
    command_lease_revision bigint;
    command_resource_generation_id uuid;
    live_server_generation bigint;
    live_server_lease_id text;
    live_server_provider_ref text;
    live_lease_revision bigint;
    live_lease_server_id text;
    live_resource_generation_id uuid;
BEGIN
    envelope_version := NEW.command_json->>'schema_version';
    execution_authority := NEW.command_json->>'execution_authority';
    IF envelope_version IS NULL
       OR envelope_version NOT IN (
            'techstack.provider-control-operation/v1',
            'homelab.provider-control-operation/v1'
       )
       OR execution_authority IS NULL
       OR execution_authority NOT IN (
            'techstack_provider_control',
            'homelab_provider_control'
       ) THEN
        RETURN NEW;
    END IF;

    pinned_server_generation :=
        NULLIF(NEW.command_json->>'runtime_server_generation', '')::bigint;
    IF pinned_server_generation IS NULL OR pinned_server_generation < 1 THEN
        RAISE EXCEPTION 'native provider operation requires a positive runtime server generation pin'
            USING ERRCODE = '55000';
    END IF;
    IF TG_OP = 'UPDATE'
       AND NEW.command_json->>'runtime_server_generation'
            IS DISTINCT FROM OLD.command_json->>'runtime_server_generation' THEN
        RAISE EXCEPTION 'provider operation runtime server generation pin is immutable'
            USING ERRCODE = '55000';
    END IF;
    IF TG_OP = 'UPDATE' THEN
        RETURN NEW;
    END IF;

    command_server_id := NEW.command_json #>> '{command,runtime_server_id}';
    command_provider_id := NEW.command_json #>> '{command,provider_id}';
    command_lease_revision :=
        (NEW.command_json #>> '{command,lease_revision}')::bigint;
    command_resource_generation_id :=
        (NEW.command_json #>> '{command,resource_generation_id}')::uuid;

    SELECT server.generation, server.lease_id, server.provider_ref
    INTO live_server_generation, live_server_lease_id, live_server_provider_ref
    FROM servers AS server
    WHERE server.tenant_id = NEW.tenant_id
      AND server.id = command_server_id
    FOR SHARE OF server;

    SELECT lease.lease_revision, lease.server_id, lease.resource_generation_id
    INTO live_lease_revision, live_lease_server_id, live_resource_generation_id
    FROM techstack_vm_leases AS lease
    WHERE lease.tenant_id = NEW.tenant_id
      AND lease.id = NEW.lease_id;

    IF live_server_generation IS DISTINCT FROM pinned_server_generation
       OR live_server_lease_id IS DISTINCT FROM NEW.lease_id
       OR NULLIF(LOWER(BTRIM(command_provider_id)), '') IS NULL
       OR NULLIF(LOWER(BTRIM(live_server_provider_ref)), '')
            IS DISTINCT FROM NULLIF(LOWER(BTRIM(command_provider_id)), '')
       OR live_lease_revision IS DISTINCT FROM command_lease_revision
       OR live_lease_server_id IS DISTINCT FROM command_server_id
       OR live_resource_generation_id IS DISTINCT FROM command_resource_generation_id THEN
        RAISE EXCEPTION 'provider operation runtime server generation pin is stale'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION provider_operation_runtime_generation_guard() FROM PUBLIC;

-- 2c. provider_control_refresh_runnable_tenant (035)
CREATE OR REPLACE FUNCTION provider_control_refresh_runnable_tenant()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    operation_tenant_id text := COALESCE(NEW.tenant_id, OLD.tenant_id);
    scoped_tenant_id text := NULLIF(current_setting('app.tenant_id', true), '');
    runnable_count integer;
BEGIN
    IF operation_tenant_id IS NULL OR BTRIM(operation_tenant_id) = '' THEN
        RAISE EXCEPTION 'provider-control runnable tenant refresh requires an exact tenant'
            USING ERRCODE = '55000';
    END IF;
    IF scoped_tenant_id IS DISTINCT FROM operation_tenant_id THEN
        RAISE EXCEPTION 'provider-control runnable tenant refresh requires the exact tenant scope'
            USING ERRCODE = '42501';
    END IF;

    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(
            'providercontrol.runnable-tenant/v1:' || operation_tenant_id,
            0
        )
    );

    SELECT COUNT(*)::integer
    INTO runnable_count
    FROM provider_operations AS operation
    WHERE operation.tenant_id = operation_tenant_id
      AND operation.command_json->>'schema_version' IN (
            'techstack.provider-control-operation/v1',
            'homelab.provider-control-operation/v1'
      )
      AND operation.command_json->>'execution_authority' IN (
            'techstack_provider_control',
            'homelab_provider_control'
      )
      AND CASE
          WHEN jsonb_typeof(operation.command_json->'runtime_server_generation') = 'number'
           AND operation.command_json->>'runtime_server_generation' ~ '^[1-9][0-9]{0,18}$'
          THEN (operation.command_json->>'runtime_server_generation')::numeric
                <= 9223372036854775807
          ELSE false
      END
      AND operation.provision_dispatch_mode <> 'blocked'
      AND operation.status = 'pending'
      AND operation.phase NOT IN ('planned', 'present', 'absent', 'failed', 'denied')
      AND NOT EXISTS (
          SELECT 1
          FROM provider_operation_resource_free_terminalizations AS terminalization
          WHERE terminalization.tenant_id = operation.tenant_id
            AND terminalization.operation_id = operation.operation_id
            AND terminalization.head_sequence = operation.head_sequence
            AND terminalization.head_receipt_digest =
                  operation.head_receipt_digest
      )
      AND NOT (
          operation.operation = 'provision'
          AND operation.phase = 'accepted'
          AND EXISTS (
              SELECT 1
              FROM provider_provision_dispatch_guards AS dispatch_guard
              WHERE dispatch_guard.tenant_id = operation.tenant_id
                AND dispatch_guard.lease_id = operation.lease_id
                AND dispatch_guard.resource_generation_id =
                    (operation.command_json #>> '{command,resource_generation_id}')::uuid
                AND (
                    dispatch_guard.operation_id <> operation.operation_id
                    OR dispatch_guard.dispatch_mode = 'at_most_once_dispatch_manual_reconcile'
                    OR dispatch_guard.guard_origin = 'migration_quarantine'
                )
          )
          AND NOT (
              EXISTS (
                  SELECT 1
                  FROM provider_provision_resolution_decisions AS decision
                  WHERE decision.tenant_id = operation.tenant_id
                    AND decision.operation_id = operation.operation_id
                    AND decision.expected_head_sequence = operation.head_sequence
                    AND decision.expected_head_receipt_digest =
                          operation.head_receipt_digest
                    AND decision.outcome = 'no_candidate_observed'
              )
              AND EXISTS (
                  SELECT 1
                  FROM techstack_vm_leases AS lease
                  JOIN servers AS server
                    ON server.tenant_id = lease.tenant_id
                   AND server.id = lease.server_id
                  WHERE lease.tenant_id = operation.tenant_id
                    AND lease.id = operation.lease_id
                    AND (
                        lease.cancelled_at IS NOT NULL
                        OR lease.desired_state = 'absent'
                        OR server.desired_state = 'absent'
                        OR server.lifecycle_state = 'decommissioning'
                    )
              )
          )
      );

    IF runnable_count > 0 THEN
        INSERT INTO provider_control_runnable_tenants (
            tenant_id,
            runnable_operation_count,
            refreshed_at
        ) VALUES (
            operation_tenant_id,
            runnable_count,
            clock_timestamp()
        )
        ON CONFLICT (tenant_id) DO UPDATE
        SET runnable_operation_count = EXCLUDED.runnable_operation_count,
            refreshed_at = EXCLUDED.refreshed_at;
    ELSE
        DELETE FROM provider_control_runnable_tenants
        WHERE tenant_id = operation_tenant_id;
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$;
REVOKE ALL ON FUNCTION provider_control_refresh_runnable_tenant() FROM PUBLIC;

-- 2d. managed_runtime_capacity_reservation_validate_insert (055 + 205 support
--     replacement temporary reservation)
CREATE OR REPLACE FUNCTION managed_runtime_capacity_reservation_validate_insert()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    operation_kind text;
    operation_status text;
    operation_phase text;
    operation_head_sequence bigint;
    operation_idempotency_key text;
    operation_schema_version text;
    operation_execution_authority text;
    operation_generation_id text;
    operation_provider_id text;
    live_owner_subject_id text;
    live_provider_id text;
    live_resource_generation_id uuid;
    live_execution_authority text;
    held_capacity integer;
    expected_policy_digest text;
    linked_replacement managed_runtime_support_replacements%ROWTYPE;
    active_temp_count integer;
BEGIN
    IF NEW.reservation_origin NOT IN ('native_admission', 'support_replacement_temporary')
       OR NEW.reservation_mode NOT IN ('limited', 'unlimited')
       OR NEW.operation_id IS NULL THEN
        RAISE EXCEPTION 'new managed runtime capacity reservation must be native admission or support replacement custody'
            USING ERRCODE = '55000';
    END IF;

    SELECT
        operation.operation,
        operation.status,
        operation.phase,
        operation.head_sequence,
        operation.idempotency_key,
        operation.command_json->>'schema_version',
        operation.command_json->>'execution_authority',
        operation.command_json #>> '{command,resource_generation_id}',
        operation.command_json #>> '{command,provider_id}'
    INTO
        operation_kind,
        operation_status,
        operation_phase,
        operation_head_sequence,
        operation_idempotency_key,
        operation_schema_version,
        operation_execution_authority,
        operation_generation_id,
        operation_provider_id
    FROM provider_operations AS operation
    WHERE operation.tenant_id = NEW.tenant_id
      AND operation.operation_id = NEW.operation_id
      AND operation.lease_id = NEW.lease_id;

    IF NOT FOUND
       OR operation_kind IS DISTINCT FROM 'provision'
       OR operation_status IS DISTINCT FROM 'pending'
       OR operation_phase IS DISTINCT FROM 'requested'
       OR operation_head_sequence IS DISTINCT FROM 1
       OR operation_idempotency_key IS DISTINCT FROM NEW.admission_idempotency_key
       OR operation_schema_version IS NULL
       OR operation_schema_version NOT IN (
            'techstack.provider-control-operation/v1',
            'homelab.provider-control-operation/v1'
       )
       OR operation_execution_authority IS NULL
       OR operation_execution_authority NOT IN (
            'techstack_provider_control',
            'homelab_provider_control'
       )
       OR operation_generation_id IS DISTINCT FROM NEW.resource_generation_id::text
       OR operation_provider_id IS DISTINCT FROM NEW.provider_id THEN
        RAISE EXCEPTION 'capacity reservation must bind the initial native provision operation'
            USING ERRCODE = '55000';
    END IF;

    SELECT
        lease.owner_subject_id,
        COALESCE(
            NULLIF(LOWER(BTRIM(lease.provider_id)), ''),
            NULLIF(LOWER(BTRIM(lease.lease_json->'resource'->>'provider_id')), '')
        ),
        lease.resource_generation_id,
        authority.execution_authority
    INTO
        live_owner_subject_id,
        live_provider_id,
        live_resource_generation_id,
        live_execution_authority
    FROM techstack_vm_leases AS lease
    JOIN runtime_lease_execution_authorities AS authority
      ON authority.tenant_id = lease.tenant_id
     AND authority.lease_id = lease.id
    WHERE lease.tenant_id = NEW.tenant_id
      AND lease.id = NEW.lease_id;

    IF NOT FOUND
       OR live_owner_subject_id IS DISTINCT FROM NEW.owner_subject_id
       OR live_provider_id IS DISTINCT FROM NEW.provider_id
       OR live_resource_generation_id IS DISTINCT FROM NEW.resource_generation_id
       OR live_execution_authority IS NULL
       OR live_execution_authority NOT IN (
            'techstack_provider_control',
            'homelab_provider_control'
       ) THEN
        RAISE EXCEPTION 'capacity reservation must bind the live native lease owner and generation'
            USING ERRCODE = '55000';
    END IF;

    PERFORM pg_advisory_xact_lock(
        hashtext(NEW.tenant_id),
        hashtext('providercontrol.capacity:owner_subject:' || NEW.owner_subject_id)
    );

    IF NEW.reservation_origin = 'support_replacement_temporary' THEN
        IF NEW.provider_id IS DISTINCT FROM 'ionos'
           OR NEW.reservation_mode IS DISTINCT FROM 'limited'
           OR NEW.capacity_limit IS DISTINCT FROM 1
           OR NEW.policy_source IS DISTINCT FROM 'support_replacement:ionos_memory_cube_s/v1' THEN
            RAISE EXCEPTION 'support replacement temporary reservation rejected outside closed policy'
                USING ERRCODE = '55000';
        END IF;

        expected_policy_digest := managed_runtime_capacity_policy_digest(
            NEW.tenant_id,
            NEW.provider_id,
            'owner_subject',
            NEW.owner_subject_id,
            NEW.reservation_mode,
            COALESCE(NEW.capacity_limit, 0),
            NEW.policy_source
        );
        IF NEW.policy_digest IS DISTINCT FROM expected_policy_digest THEN
            RAISE EXCEPTION 'support replacement temporary capacity policy digest mismatch'
                USING ERRCODE = '55000';
        END IF;

        SELECT *
        INTO linked_replacement
        FROM managed_runtime_support_replacements AS sr
        WHERE sr.tenant_id = NEW.tenant_id
          AND sr.owner_subject_id = NEW.owner_subject_id
          AND sr.destination_lease_id = NEW.lease_id
          AND sr.destination_resource_generation_id = NEW.resource_generation_id::text
          AND sr.destination_operation_id = NEW.operation_id
          AND sr.customer_accepted_at IS NOT NULL
          AND sr.no_charge_confirmed = true
          AND sr.target_product_slug = 'ionos-cube-standard-memory'
          AND sr.phase IN ('accepted', 'destination_admitted', 'staging')
        LIMIT 1;

        IF NOT FOUND THEN
            RAISE EXCEPTION 'support replacement temporary capacity reservation requires active customer-accepted Memory Cube S replacement'
                USING ERRCODE = '55000';
        END IF;

        IF EXISTS (
            SELECT 1
            FROM managed_runtime_capacity_release_facts AS rel
            WHERE rel.tenant_id = NEW.tenant_id
              AND rel.lease_id = linked_replacement.source_lease_id
              AND rel.resource_generation_id::text = linked_replacement.source_resource_generation_id
        ) THEN
            RAISE EXCEPTION 'support replacement temporary capacity reservation requires unreleased source cube'
                USING ERRCODE = '55000';
        END IF;

        SELECT count(*)
        INTO active_temp_count
        FROM managed_runtime_capacity_reservations AS reservation
        WHERE reservation.tenant_id = NEW.tenant_id
          AND reservation.owner_subject_id = NEW.owner_subject_id
          AND reservation.reservation_origin = 'support_replacement_temporary'
          AND NOT EXISTS (
              SELECT 1
              FROM managed_runtime_capacity_release_facts AS release
              WHERE release.tenant_id = reservation.tenant_id
                AND release.lease_id = reservation.lease_id
                AND release.resource_generation_id = reservation.resource_generation_id
          )
          AND NOT EXISTS (
              SELECT 1
              FROM managed_runtime_support_replacements AS sr
              JOIN managed_runtime_capacity_release_facts AS src_release
                ON src_release.tenant_id = sr.tenant_id
               AND src_release.lease_id = sr.source_lease_id
               AND src_release.resource_generation_id::text = sr.source_resource_generation_id
              WHERE sr.tenant_id = reservation.tenant_id
                AND sr.destination_lease_id = reservation.lease_id
                AND sr.destination_resource_generation_id = reservation.resource_generation_id::text
                AND sr.phase = 'completed'
          );

        IF active_temp_count > 0 THEN
            RAISE EXCEPTION 'concurrent support replacement temporary reservations exceed boundary'
                USING ERRCODE = '55000';
        END IF;

        RETURN NEW;
    END IF;

    IF NEW.reservation_mode = 'limited' THEN
        SELECT count(*)
        INTO held_capacity
        FROM managed_runtime_capacity_reservations AS reservation
        WHERE reservation.tenant_id = NEW.tenant_id
          AND reservation.owner_subject_id = NEW.owner_subject_id
          AND NOT EXISTS (
              SELECT 1
              FROM managed_runtime_capacity_release_facts AS release
              WHERE release.tenant_id = reservation.tenant_id
                AND release.lease_id = reservation.lease_id
                AND release.resource_generation_id = reservation.resource_generation_id
          )
          AND NOT EXISTS (
              SELECT 1
              FROM managed_runtime_capacity_quarantine_retirements AS retirement
              WHERE retirement.tenant_id = reservation.tenant_id
                AND retirement.lease_id = reservation.lease_id
                AND retirement.resource_generation_id = reservation.resource_generation_id
          )
          AND (
              reservation.reservation_origin <> 'support_replacement_temporary'
              OR EXISTS (
                  SELECT 1
                  FROM managed_runtime_support_replacements AS sr
                  JOIN managed_runtime_capacity_release_facts AS src_release
                    ON src_release.tenant_id = sr.tenant_id
                   AND src_release.lease_id = sr.source_lease_id
                   AND src_release.resource_generation_id::text = sr.source_resource_generation_id
                  WHERE sr.tenant_id = reservation.tenant_id
                    AND sr.destination_lease_id = reservation.lease_id
                    AND sr.destination_resource_generation_id = reservation.resource_generation_id::text
              )
          );
        IF held_capacity >= NEW.capacity_limit THEN
            RAISE EXCEPTION 'managed runtime capacity reservation exceeds the authoritative owner limit'
                USING ERRCODE = '55000';
        END IF;
    END IF;

    expected_policy_digest := managed_runtime_capacity_policy_digest(
        NEW.tenant_id,
        NEW.provider_id,
        'owner_subject',
        NEW.owner_subject_id,
        NEW.reservation_mode,
        COALESCE(NEW.capacity_limit, 0),
        NEW.policy_source
    );
    IF NEW.policy_digest IS DISTINCT FROM expected_policy_digest THEN
        RAISE EXCEPTION 'managed runtime capacity policy digest does not match its canonical projection'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION managed_runtime_capacity_reservation_validate_insert() FROM PUBLIC;

-- 2e. provider_control_refresh_decommission_wait_tenant (071)
CREATE OR REPLACE FUNCTION provider_control_refresh_decommission_wait_tenant()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    wait_tenant_id text := COALESCE(NEW.tenant_id, OLD.tenant_id);
    scoped_tenant_id text := NULLIF(current_setting('app.tenant_id', true), '');
    wait_count integer;
    first_resume_at timestamptz;
BEGIN
    IF wait_tenant_id IS NULL OR BTRIM(wait_tenant_id) = '' THEN
        RAISE EXCEPTION 'provider decommission wait refresh requires an exact tenant'
            USING ERRCODE = '55000';
    END IF;
    IF scoped_tenant_id IS DISTINCT FROM wait_tenant_id THEN
        RAISE EXCEPTION 'provider decommission wait refresh requires the exact tenant scope'
            USING ERRCODE = '42501';
    END IF;

    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(
            'providercontrol.decommission-wait-tenant/v1:' || wait_tenant_id,
            0
        )
    );

    SELECT COUNT(*)::integer,
        MIN(
            CASE job.state
                WHEN 'pending' THEN job.scheduled_for
                WHEN 'running' THEN job.updated_at + interval '3 seconds'
            END
        )
    INTO wait_count, first_resume_at
    FROM jobs AS job
    WHERE job.tenant_id = wait_tenant_id
      AND job.type = 'destroy'
      AND job.state IN ('pending', 'running')
      AND job.result_json->'managed_provider_decommission_recovery'->>'schema' IN (
          'techstack.managed-provider-decommission-recovery/v1',
          'homelab.managed-provider-decommission-recovery/v1'
      )
      AND job.result_json->'managed_provider_decommission_recovery'->>'tenant_id' = job.tenant_id
      AND job.result_json->'managed_provider_decommission_recovery'->>'stack_id' = job.stack_id;

    IF wait_count > 0 AND first_resume_at IS NOT NULL THEN
        INSERT INTO provider_decommission_wait_tenants (
            tenant_id,
            recovery_destroy_count,
            earliest_resume_at,
            refreshed_at
        ) VALUES (
            wait_tenant_id,
            wait_count,
            first_resume_at,
            clock_timestamp()
        )
        ON CONFLICT (tenant_id) DO UPDATE
        SET recovery_destroy_count = EXCLUDED.recovery_destroy_count,
            earliest_resume_at = EXCLUDED.earliest_resume_at,
            refreshed_at = EXCLUDED.refreshed_at;
    ELSE
        DELETE FROM provider_decommission_wait_tenants
        WHERE tenant_id = wait_tenant_id;
    END IF;

    RETURN COALESCE(NEW, OLD);
END;
$$;
REVOKE ALL ON FUNCTION provider_control_refresh_decommission_wait_tenant() FROM PUBLIC;

-- Note: provider_control_list_stale_capacity_recovery_candidates (093) already
-- reads only provider_stale_capacity_recovery_candidates without techstack_*
-- literals and must NOT be overwritten with 078's legacy cross-tenant join.

-- 2f. substrate_guest_custody_valid (107)
CREATE OR REPLACE FUNCTION substrate_guest_custody_valid(p_tenant text, p_lease text, p_generation text, p_operation text)
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
 SELECT EXISTS (SELECT 1 FROM substrate_guest_leases g
 JOIN techstack_vm_leases l ON l.tenant_id=g.tenant_id AND l.id=g.lease_id
 JOIN servers s ON s.tenant_id=g.tenant_id AND s.id=g.server_id AND s.lease_id=g.lease_id
 JOIN provider_operations p ON p.tenant_id=g.tenant_id AND p.operation_id=p_operation AND p.lease_id=g.lease_id
 WHERE p_tenant=current_setting('app.tenant_id',true)
 AND g.tenant_id=p_tenant AND g.lease_id=p_lease AND g.resource_generation_id::text=p_generation
 AND l.resource_generation_id=g.resource_generation_id AND l.server_id=g.server_id AND l.provider_id='proxmox'
 AND l.lease_json->>'custody_class'='customer_substrate'
 AND l.lease_json->>'billing_mode'='local'
 AND l.lease_json->>'lifecycle_class'='one_time'
 AND l.lease_json->>'recreate_policy'='never'
 AND s.owner_subject_id=l.owner_subject_id
 AND s.provider_id='proxmox' AND s.offering='substrate_vm'
 AND s.provider_target_ref=g.substrate_server_id || '/' || g.guest_id::text
 AND l.lease_json #>> '{metadata,substrate_server_id}'=g.substrate_server_id
 AND l.lease_json #>> '{metadata,substrate_guest_id}'=g.guest_id::text
 AND l.lease_json #>> '{resource,engine_vm_id}'=g.substrate_server_id || '/' || g.guest_id::text
 AND p.command_json->>'schema_version' IN (
     'techstack.provider-control-operation/v1',
     'homelab.provider-control-operation/v1'
 )
 AND p.command_json->>'execution_authority' IN (
     'techstack_provider_control',
     'homelab_provider_control'
 )
 AND p.command_json #>> '{command,provider_id}'='proxmox'
 AND p.command_json #>> '{command,resource_generation_id}'=g.resource_generation_id::text
 AND p.command_json #>> '{command,runtime_server_id}'=g.server_id
 AND p.command_json #>> '{execution_profile,credential_mode}'='worker_held'
 AND (p.operation<>'provision' OR g.operation_id=p.operation_id));
$$;
REVOKE ALL ON FUNCTION substrate_guest_custody_valid(text, text, text, text) FROM PUBLIC;

-- 2g. provider_operation_require_capacity_reservation (107 + 205 support
--     replacement temporary reservation)
CREATE OR REPLACE FUNCTION provider_operation_require_capacity_reservation()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    operation_generation_id text;
    matched_origin text;
BEGIN
    IF NEW.command_json #>> '{command,provider_id}' = 'proxmox' THEN
        IF NOT substrate_guest_custody_valid(NEW.tenant_id, NEW.lease_id, NEW.command_json #>> '{command,resource_generation_id}', NEW.operation_id) THEN
            RAISE EXCEPTION 'native substrate operation requires atomic guest custody' USING ERRCODE='55000';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.operation IS DISTINCT FROM 'provision'
       OR NEW.command_json->>'schema_version' IS NULL
       OR NEW.command_json->>'schema_version' NOT IN (
            'techstack.provider-control-operation/v1',
            'homelab.provider-control-operation/v1'
       )
       OR NEW.command_json->>'execution_authority' IS NULL
       OR NEW.command_json->>'execution_authority' NOT IN (
            'techstack_provider_control',
            'homelab_provider_control'
       ) THEN
        RETURN NEW;
    END IF;

    operation_generation_id := NEW.command_json #>> '{command,resource_generation_id}';
    SELECT reservation.reservation_origin
    INTO matched_origin
    FROM managed_runtime_capacity_reservations AS reservation
    WHERE reservation.tenant_id = NEW.tenant_id
      AND reservation.lease_id = NEW.lease_id
      AND reservation.operation_id = NEW.operation_id
      AND reservation.resource_generation_id::text = operation_generation_id
      AND reservation.reservation_origin IN ('native_admission', 'support_replacement_temporary')
      AND reservation.reservation_mode IN ('limited', 'unlimited')
      AND reservation.policy_source IN (
          'edge_v2_entitlement+signed_budget:cloud.runtime.credits#managed_servers',
          'static_release_manifest:selfhost-oss',
          'support_replacement:ionos_memory_cube_s/v1'
      );
    IF NOT FOUND THEN
        RAISE EXCEPTION 'native provision operation requires an atomic managed runtime capacity reservation'
            USING ERRCODE = '55000';
    END IF;

    IF matched_origin = 'support_replacement_temporary' AND NOT EXISTS (
        SELECT 1
        FROM managed_runtime_support_replacements AS sr
        WHERE sr.tenant_id = NEW.tenant_id
          AND sr.destination_lease_id = NEW.lease_id
          AND sr.destination_resource_generation_id = operation_generation_id
          AND sr.destination_operation_id = NEW.operation_id
          AND sr.customer_accepted_at IS NOT NULL
          AND sr.no_charge_confirmed = true
          AND sr.target_product_slug = 'ionos-cube-standard-memory'
    ) THEN
        RAISE EXCEPTION 'support replacement provision operation lacks matching authorization'
            USING ERRCODE = '55000';
    END IF;

    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION provider_operation_require_capacity_reservation() FROM PUBLIC;

-- 2h. managed_runtime_capacity_execution_claim_guard (107 + 205 support
--     replacement temporary reservation)
CREATE OR REPLACE FUNCTION managed_runtime_capacity_execution_claim_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    operation_kind text;
    operation_lease_id text;
    operation_schema_version text;
    operation_execution_authority text;
    operation_generation_id text;
    operation_provider_id text;
    operation_profile_provider_id text;
BEGIN
    IF NEW.state IS DISTINCT FROM 'active' THEN
        RETURN NEW;
    END IF;

    SELECT
        operation.operation,
        operation.lease_id,
        operation.command_json->>'schema_version',
        operation.command_json->>'execution_authority',
        operation.command_json #>> '{command,resource_generation_id}',
        operation.command_json #>> '{command,provider_id}',
        operation.command_json #>> '{execution_profile,provider_id}'
    INTO
        operation_kind,
        operation_lease_id,
        operation_schema_version,
        operation_execution_authority,
        operation_generation_id,
        operation_provider_id,
        operation_profile_provider_id
    FROM provider_operations AS operation
    WHERE operation.tenant_id = NEW.tenant_id
      AND operation.operation_id = NEW.operation_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'provider execution claim has no durable operation'
            USING ERRCODE = '55000';
    END IF;
    IF operation_provider_id = 'proxmox' THEN
        IF NOT substrate_guest_custody_valid(NEW.tenant_id, operation_lease_id, operation_generation_id, NEW.operation_id) THEN
            RAISE EXCEPTION 'substrate execution requires exact guest custody' USING ERRCODE='55000';
        END IF;
        RETURN NEW;
    END IF;
    IF operation_kind NOT IN ('provision', 'reconcile') THEN
        RETURN NEW;
    END IF;
    IF operation_schema_version IS NULL
       OR operation_schema_version NOT IN (
            'techstack.provider-control-operation/v1',
            'homelab.provider-control-operation/v1'
       )
       OR operation_execution_authority IS NULL
       OR operation_execution_authority NOT IN (
            'techstack_provider_control',
            'homelab_provider_control'
       )
       OR operation_provider_id NOT IN ('ionos', 'centron')
       OR operation_profile_provider_id IS DISTINCT FROM operation_provider_id THEN
        RAISE EXCEPTION 'provider execution claim is blocked by missing native managed runtime capacity authority'
            USING ERRCODE = '55000';
    END IF;

    PERFORM 1
    FROM managed_runtime_capacity_reservations AS reservation
    WHERE reservation.tenant_id = NEW.tenant_id
      AND reservation.lease_id = operation_lease_id
      AND reservation.resource_generation_id::text = operation_generation_id
      AND reservation.provider_id = operation_provider_id
      AND reservation.reservation_origin IN ('native_admission', 'support_replacement_temporary')
      AND reservation.reservation_mode IN ('limited', 'unlimited')
      AND reservation.policy_source IN (
          'edge_v2_entitlement+signed_budget:cloud.runtime.credits#managed_servers',
          'static_release_manifest:selfhost-oss',
          'support_replacement:ionos_memory_cube_s/v1'
      )
      AND (
          operation_kind <> 'provision'
          OR reservation.operation_id = NEW.operation_id
      );
    IF NOT FOUND THEN
        RAISE EXCEPTION 'provider execution claim is blocked by missing native managed runtime capacity authority'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION managed_runtime_capacity_execution_claim_guard() FROM PUBLIC;

-- 2i. provider_control_list_never_enrolled_runtime_candidates (114 + 205
--     active support replacement exclusion)
CREATE OR REPLACE FUNCTION provider_control_list_never_enrolled_runtime_candidates(
    after_tenant_id text,
    after_lease_id text,
    enrollment_window_seconds integer,
    requested_limit integer
)
RETURNS TABLE (
    tenant_id text,
    owner_subject_id text,
    lease_id text,
    stack_id text,
    reserved_at timestamptz
)
LANGUAGE plpgsql
SECURITY DEFINER
STABLE
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF requested_limit IS NULL OR requested_limit < 1 OR requested_limit > 101 THEN
        RAISE EXCEPTION 'provider-control never-enrolled runtime limit must be between 1 and 101'
            USING ERRCODE = '22023';
    END IF;
    IF enrollment_window_seconds IS NULL OR enrollment_window_seconds < 60 OR enrollment_window_seconds > 86400 THEN
        RAISE EXCEPTION 'provider-control never-enrolled enrollment window must be between 60 and 86400 seconds'
            USING ERRCODE = '22023';
    END IF;

    RETURN QUERY
    WITH candidates AS (
        SELECT
            reservation.tenant_id AS tenant_id,
            reservation.owner_subject_id AS owner_subject_id,
            reservation.lease_id AS lease_id,
            reservation.reserved_at AS reserved_at,
            COALESCE(lease.lease_json->'metadata'->>'stack_id', '') AS stack_id
        FROM managed_runtime_capacity_reservations AS reservation
        JOIN techstack_vm_leases AS lease
          ON lease.tenant_id = reservation.tenant_id
         AND lease.id = reservation.lease_id
         AND lease.resource_generation_id = reservation.resource_generation_id
        JOIN runtime_lease_execution_authorities AS authority
          ON authority.tenant_id = lease.tenant_id
         AND authority.lease_id = lease.id
         AND authority.execution_authority IN (
                'techstack_provider_control',
                'homelab_provider_control'
         )
        WHERE NOT EXISTS (
                  SELECT 1
                  FROM managed_runtime_capacity_release_facts AS release
                  WHERE release.tenant_id = reservation.tenant_id
                    AND release.lease_id = reservation.lease_id
                    AND release.resource_generation_id = reservation.resource_generation_id
              )
          AND lease.cancelled_at IS NULL
          AND lease.desired_state <> 'absent'
          AND NOT EXISTS (
                  SELECT 1
                  FROM servers AS server
                  WHERE server.tenant_id = lease.tenant_id
                    AND server.id = lease.server_id
                    AND server.lease_id = lease.id
                    AND server.desired_state = 'absent'
                    AND (
                        server.lifecycle_state IN ('decommissioning', 'decommissioned')
                        OR server.decommissioned_at IS NOT NULL
                    )
              )
          AND reservation.reserved_at < clock_timestamp() - make_interval(secs => enrollment_window_seconds)
          AND NOT EXISTS (
                  SELECT 1
                  FROM servers AS server
                  WHERE server.tenant_id = lease.tenant_id
                    AND server.id = lease.server_id
                    AND server.lease_id = lease.id
                    AND (
                        server.worker_id IS NOT NULL
                        OR server.last_heartbeat_at IS NOT NULL
                        OR server.lifecycle_state = 'active'
                        OR server.connection_state IN ('connected', 'degraded')
                    )
              )
          AND NOT EXISTS (
                  SELECT 1
                  FROM managed_runtime_support_replacements AS sr
                  WHERE sr.tenant_id = lease.tenant_id
                    AND sr.phase IN (
                        'destination_admitted',
                        'staging',
                        'cutover_in_progress',
                        'cutover_verified',
                        'source_decommissioning',
                        'rolling_back',
                        'fenced_conflict'
                    )
                    AND (
                        sr.destination_lease_id = lease.id
                        OR sr.source_lease_id = lease.id
                    )
              )
    )
    SELECT
        candidate.tenant_id,
        candidate.owner_subject_id,
        candidate.lease_id,
        candidate.stack_id,
        candidate.reserved_at
    FROM candidates AS candidate
    WHERE candidate.stack_id <> ''
      AND NOT EXISTS (
              SELECT 1
              FROM jobs AS job
              WHERE job.tenant_id = candidate.tenant_id
                AND job.stack_id = candidate.stack_id
                AND job.state IN ('pending', 'running', 'waiting')
          )
      AND (candidate.tenant_id, candidate.lease_id) > (
          COALESCE(BTRIM(after_tenant_id), ''),
          COALESCE(BTRIM(after_lease_id), '')
      )
    ORDER BY candidate.tenant_id, candidate.lease_id
    LIMIT requested_limit;
END;
$$;
REVOKE ALL ON FUNCTION provider_control_list_never_enrolled_runtime_candidates(text, text, integer, integer) FROM PUBLIC;

-- 2j. provider_operation_dispatch_mode_insert_guard (137)
CREATE OR REPLACE FUNCTION provider_operation_dispatch_mode_insert_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    catalog_dispatch_mode text;
    snapshot_dispatch_mode text;
    catalog_adapter_manifest_hash text;
    snapshot_adapter_manifest_hash text;
BEGIN
    IF NEW.command_json->>'schema_version' IS NULL
       OR NEW.command_json->>'schema_version' NOT IN (
            'techstack.provider-control-operation/v1',
            'homelab.provider-control-operation/v1'
       )
       OR NEW.command_json->>'execution_authority' IS NULL
       OR NEW.command_json->>'execution_authority' NOT IN (
            'techstack_provider_control',
            'homelab_provider_control'
       ) THEN
        RAISE EXCEPTION 'new provider operation requires the native provider-control envelope'
            USING ERRCODE = '55000';
    END IF;

    snapshot_dispatch_mode := NEW.command_json #>> '{execution_profile,provision_dispatch_mode}';
    snapshot_adapter_manifest_hash := NEW.command_json #>> '{execution_profile,adapter_manifest_hash}';
    IF NEW.provision_dispatch_mode = 'blocked'
       OR snapshot_dispatch_mode IS DISTINCT FROM NEW.provision_dispatch_mode
       OR snapshot_adapter_manifest_hash IS NULL
       OR snapshot_adapter_manifest_hash !~ '^sha256:[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'provider operation must copy an executable catalog dispatch-mode pin'
            USING ERRCODE = '55000';
    END IF;

    IF NEW.command_json #>> '{execution_profile,provider_id}' = 'proxmox' THEN
        IF NEW.command_json #>> '{command,provider_id}' IS DISTINCT FROM 'proxmox'
           OR NEW.command_json #>> '{execution_profile,catalog_version}' IS DISTINCT FROM 'substrate-v1'
           OR NEW.command_json #>> '{execution_profile,adapter_id}' IS DISTINCT FROM 'proxmox-substrate-v1'
           OR NEW.command_json #>> '{execution_profile,credential_mode}' IS DISTINCT FROM 'worker_held'
           OR NEW.command_json #>> '{execution_profile,runtime_profile_id}' IS DISTINCT FROM 'customer-substrate-v1'
           OR COALESCE(NEW.command_json #>> '{execution_profile,offering_id}', '') NOT IN ('ubuntu-26.04','ubuntu-24.04','haos')
           OR NEW.provision_dispatch_mode IS DISTINCT FROM 'provider_correlation'
           OR snapshot_adapter_manifest_hash IS DISTINCT FROM 'sha256:d0d36d0ce8eca8c262240fb4e3d08603acdb9fd2ebd0182ef1584b9a4c701876' THEN
            RAISE EXCEPTION 'substrate dispatch requires the pinned worker-held profile' USING ERRCODE='55000';
        END IF;
        RETURN NEW;
    END IF;
    SELECT profile.provision_dispatch_mode, profile.adapter_manifest_hash
    INTO catalog_dispatch_mode, catalog_adapter_manifest_hash
    FROM provider_catalog_profiles AS profile
    WHERE profile.catalog_version = NEW.command_json #>> '{execution_profile,catalog_version}'
      AND profile.provider_id = NEW.command_json #>> '{execution_profile,provider_id}'
      AND profile.adapter_id = NEW.command_json #>> '{execution_profile,adapter_id}'
      AND profile.credential_mode = NEW.command_json #>> '{execution_profile,credential_mode}'
      AND profile.runtime_profile_id = NEW.command_json #>> '{execution_profile,runtime_profile_id}'
      AND profile.offering_id = NEW.command_json #>> '{execution_profile,offering_id}';
    IF catalog_dispatch_mode IS NULL
       OR catalog_dispatch_mode IS DISTINCT FROM NEW.provision_dispatch_mode
       OR catalog_adapter_manifest_hash IS DISTINCT FROM snapshot_adapter_manifest_hash THEN
        RAISE EXCEPTION 'provider operation dispatch mode does not match its immutable catalog profile'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION provider_operation_dispatch_mode_insert_guard() FROM PUBLIC;

-- 2k. public.provider_control_refresh_stale_capacity_recovery_tenant (163 no-op
--     servers UPDATE guard + 205 support replacement exclusion; restores 163's
--     pinned search_path)
CREATE OR REPLACE FUNCTION public.provider_control_refresh_stale_capacity_recovery_tenant()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    recovery_tenant_id text := COALESCE(NEW.tenant_id, OLD.tenant_id);
    scoped_tenant_id text := NULLIF(current_setting('app.tenant_id', true), '');
BEGIN
    IF recovery_tenant_id IS NULL OR BTRIM(recovery_tenant_id) = '' THEN
        RAISE EXCEPTION 'stale capacity recovery refresh requires an exact tenant'
            USING ERRCODE = '55000';
    END IF;
    IF scoped_tenant_id IS DISTINCT FROM recovery_tenant_id THEN
        RAISE EXCEPTION 'stale capacity recovery refresh requires the exact tenant scope'
            USING ERRCODE = '42501';
    END IF;

    -- Preserve migration 163's no-op UPDATE guard so Guard server heartbeats do
    -- not serialize on the tenant-wide stale-capacity recovery advisory lock.
    -- plpgsql compiles a trigger function per table, so the servers columns
    -- are resolved only on servers.
    IF TG_TABLE_NAME = 'servers' AND TG_OP = 'UPDATE' THEN
        IF OLD.tenant_id IS NOT DISTINCT FROM NEW.tenant_id
            AND OLD.id IS NOT DISTINCT FROM NEW.id
            AND OLD.lease_id IS NOT DISTINCT FROM NEW.lease_id
            AND OLD.desired_state IS NOT DISTINCT FROM NEW.desired_state
            AND OLD.lifecycle_state IS NOT DISTINCT FROM NEW.lifecycle_state
            AND OLD.decommissioned_at IS NOT DISTINCT FROM NEW.decommissioned_at THEN
            RETURN NEW;
        END IF;
    END IF;

    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(
            'providercontrol.stale-capacity-recovery/v1:' || recovery_tenant_id,
            0
        )
    );

    DELETE FROM provider_stale_capacity_recovery_candidates
    WHERE tenant_id = recovery_tenant_id;

    INSERT INTO provider_stale_capacity_recovery_candidates (
        tenant_id,
        lease_id,
        owner_subject_id,
        stack_id,
        refreshed_at
    )
    SELECT
        reservation.tenant_id,
        reservation.lease_id,
        reservation.owner_subject_id,
        lease.lease_json->'metadata'->>'stack_id',
        clock_timestamp()
    FROM managed_runtime_capacity_reservations AS reservation
    JOIN techstack_vm_leases AS lease
      ON lease.tenant_id = reservation.tenant_id
     AND lease.id = reservation.lease_id
     AND lease.resource_generation_id = reservation.resource_generation_id
    JOIN runtime_lease_execution_authorities AS authority
      ON authority.tenant_id = lease.tenant_id
     AND authority.lease_id = lease.id
     AND authority.execution_authority IN (
            'techstack_provider_control',
            'homelab_provider_control'
     )
    WHERE reservation.tenant_id = recovery_tenant_id
      AND NOT EXISTS (
          SELECT 1
          FROM managed_runtime_capacity_release_facts AS release
          WHERE release.tenant_id = reservation.tenant_id
            AND release.lease_id = reservation.lease_id
            AND release.resource_generation_id = reservation.resource_generation_id
      )
      AND NOT EXISTS (
          SELECT 1
          FROM managed_runtime_support_replacements AS sr
          WHERE sr.tenant_id = reservation.tenant_id
            AND sr.phase IN (
                'destination_admitted',
                'staging',
                'cutover_in_progress',
                'cutover_verified',
                'source_decommissioning',
                'rolling_back',
                'fenced_conflict'
            )
            AND (
                sr.source_lease_id = reservation.lease_id
                OR sr.destination_lease_id = reservation.lease_id
            )
      )
      AND (
          lease.cancelled_at IS NOT NULL
          OR lease.desired_state = 'absent'
          OR EXISTS (
              SELECT 1
              FROM servers AS server
              WHERE server.tenant_id = lease.tenant_id
                AND server.id = lease.server_id
                AND server.lease_id = lease.id
                AND server.desired_state = 'absent'
                AND (
                    server.lifecycle_state IN ('decommissioning', 'decommissioned')
                    OR server.decommissioned_at IS NOT NULL
                )
          )
      )
      AND COALESCE(lease.lease_json->'metadata'->>'stack_id', '') <> '';

    RETURN COALESCE(NEW, OLD);
END;
$$;
REVOKE ALL ON FUNCTION public.provider_control_refresh_stale_capacity_recovery_tenant() FROM PUBLIC;

-- 2l. public.techstack_backup_execution_conflicts (167)
CREATE OR REPLACE FUNCTION public.techstack_backup_execution_conflicts(p_tenant text, p_job text)
RETURNS TABLE(id text, stack_id text, type text, targets_json jsonb)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    IF session_user NOT IN ('techstack_backup_worker', 'homelab_backup_worker')
        OR p_tenant IS NULL OR p_tenant = ''
        OR p_tenant IS DISTINCT FROM current_setting('app.tenant_id',true)
        OR p_job IS NULL OR p_job = '' THEN
        RAISE EXCEPTION 'backup conflict projection requires its exact worker and tenant' USING ERRCODE='42501';
    END IF;
    IF (SELECT count(*) FROM public.jobs j WHERE j.tenant_id=p_tenant AND j.id<>p_job
        AND (j.state='running' OR (j.type='workload_relocation' AND j.result_json->'workload_relocation'->>'state'
            IN ('transferring','destination_verification_required','transfer_receipt_incomplete')))) > 500 THEN
        RAISE EXCEPTION 'backup conflict projection exceeds its bounded execution set' USING ERRCODE='54000';
    END IF;
    RETURN QUERY SELECT j.id,COALESCE(j.stack_id,''),j.type,
        jsonb_build_object('agent_id',j.payload_json->'agent_id',
            'execution_targets',CASE WHEN jsonb_typeof(j.payload_json->'execution_targets')='array' THEN
                (SELECT jsonb_agg(jsonb_build_object('stack_id',target->'stack_id','agent_id',target->'agent_id'))
                    FROM jsonb_array_elements(j.payload_json->'execution_targets') target) ELSE NULL END,
            'workload_relocation',jsonb_build_object(
                'source',jsonb_build_object('stack_id',j.payload_json#>'{workload_relocation,source,stack_id}',
                    'agent_id',j.payload_json#>'{workload_relocation,source,agent_id}'),
                'destination',jsonb_build_object('stack_id',j.payload_json#>'{workload_relocation,destination,stack_id}',
                    'agent_id',j.payload_json#>'{workload_relocation,destination,agent_id}')))
        FROM public.jobs j WHERE j.tenant_id=p_tenant AND j.id<>p_job
        AND (j.state='running' OR (j.type='workload_relocation' AND j.result_json->'workload_relocation'->>'state'
            IN ('transferring','destination_verification_required','transfer_receipt_incomplete')));
END $$;
REVOKE ALL ON FUNCTION public.techstack_backup_execution_conflicts(text, text) FROM PUBLIC;

-- 2m. public.techstack_household_inventory_owner & server (178 + 179)
-- Keep existing EXECUTE grants (techstack_application only after 179) intact.
CREATE OR REPLACE FUNCTION public.techstack_household_inventory_owner(p_tenant text, p_owner text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp AS $$
 SELECT coalesce(
   p_owner <> '' AND p_owner <> kombify_ctx.subject()
   AND p_tenant = 'usr:' || p_owner
   AND kombify_ctx.org_id() = 'usr:' || kombify_ctx.subject()
   AND (
     kombify_ctx.has_scope('techstack.household.owner.' || encode(sha256(convert_to(p_owner,'UTF8')),'hex'))
     OR kombify_ctx.has_scope('homelab.household.owner.' || encode(sha256(convert_to(p_owner,'UTF8')),'hex'))
   )
   AND (
     kombify_ctx.has_scope('techstack.household.collection')
     OR kombify_ctx.has_scope('homelab.household.collection')
     OR EXISTS (
       SELECT 1 FROM unnest(kombify_ctx.scopes()) scope
       WHERE scope ~ '^(techstack|homelab)[.]household[.]server[.][0-9a-f]{64}$'
     )
   ), false)
$$;

CREATE OR REPLACE FUNCTION public.techstack_household_inventory_server(p_tenant text, p_owner text, p_server text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp AS $$
 SELECT public.techstack_household_inventory_owner(p_tenant,p_owner)
   AND (
     kombify_ctx.has_scope('techstack.household.collection')
     OR kombify_ctx.has_scope('homelab.household.collection')
     OR kombify_ctx.has_scope('techstack.household.server.' || encode(sha256(convert_to(p_server,'UTF8')),'hex'))
     OR kombify_ctx.has_scope('homelab.household.server.' || encode(sha256(convert_to(p_server,'UTF8')),'hex'))
   )
$$;

-- ---------------------------------------------------------------------------
-- 3. Widen RLS policies for household scopes & session_user roles
-- ---------------------------------------------------------------------------

-- Preserve contract/191 verified-context enforcement if already applied;
-- otherwise preserve 178's verified_context() IS NULL fallback until 191 runs.
DO $household_stacks_policy$
DECLARE
    contract_191_applied boolean := false;
BEGIN
    IF pg_catalog.to_regclass('public.schema_contract_migrations') IS NOT NULL THEN
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM public.schema_contract_migrations WHERE version = ''191_inventory_verified_context.sql'')'
            INTO contract_191_applied;
    END IF;
    IF contract_191_applied THEN
        EXECUTE $pol$
            ALTER POLICY household_inventory_read ON public.stacks
             USING (tenant_id = (SELECT kombify_ctx.org_id()) OR (
               public.techstack_household_inventory_owner(tenant_id,owner_subject_id)
               AND (kombify_ctx.has_scope('techstack.household.collection')
                    OR kombify_ctx.has_scope('homelab.household.collection')
                    OR EXISTS (
                 SELECT 1 FROM public.servers authorized_server
                 WHERE authorized_server.tenant_id=stacks.tenant_id
                   AND authorized_server.owner_subject_id=stacks.owner_subject_id
                   AND authorized_server.stack_id=stacks.id
                   AND public.techstack_household_inventory_server(authorized_server.tenant_id,authorized_server.owner_subject_id,authorized_server.id)
               ))))
        $pol$;
    ELSE
        EXECUTE $pol$
            ALTER POLICY household_inventory_read ON public.stacks
             USING ((SELECT (kombify_ctx.verified_context()).subject) IS NULL OR tenant_id = (SELECT kombify_ctx.org_id()) OR (
               public.techstack_household_inventory_owner(tenant_id,owner_subject_id)
               AND (kombify_ctx.has_scope('techstack.household.collection')
                    OR kombify_ctx.has_scope('homelab.household.collection')
                    OR EXISTS (
                 SELECT 1 FROM public.servers authorized_server
                 WHERE authorized_server.tenant_id=stacks.tenant_id
                   AND authorized_server.owner_subject_id=stacks.owner_subject_id
                   AND authorized_server.stack_id=stacks.id
                   AND public.techstack_household_inventory_server(authorized_server.tenant_id,authorized_server.owner_subject_id,authorized_server.id)
               ))))
        $pol$;
    END IF;
END
$household_stacks_policy$;

ALTER POLICY onboarding_verified_identity ON user_onboarding_state
    USING (
        session_user NOT IN ('techstack_application', 'homelab_application')
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND owner_subject_id = (SELECT kombify_ctx.subject())
        )
    )
    WITH CHECK (
        session_user NOT IN ('techstack_application', 'homelab_application')
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND owner_subject_id = (SELECT kombify_ctx.subject())
        )
    );

ALTER POLICY feature_preferences_verified_identity ON feature_preferences
    USING (
        session_user NOT IN ('techstack_application', 'homelab_application')
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND subject_id = (SELECT kombify_ctx.subject())
        )
    )
    WITH CHECK (
        session_user NOT IN ('techstack_application', 'homelab_application')
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND subject_id = (SELECT kombify_ctx.subject())
        )
    );

ALTER POLICY feature_consents_verified_identity ON feature_consents
    USING (
        session_user NOT IN ('techstack_application', 'homelab_application')
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND subject_id = (SELECT kombify_ctx.subject())
        )
    )
    WITH CHECK (
        session_user NOT IN ('techstack_application', 'homelab_application')
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND subject_id = (SELECT kombify_ctx.subject())
        )
    );

ALTER POLICY operator_self_disclosures_verified_identity ON operator_self_disclosures
    USING (
        session_user NOT IN ('techstack_application', 'homelab_application')
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND owner_subject_id = (SELECT kombify_ctx.subject())
        )
    )
    WITH CHECK (
        session_user NOT IN ('techstack_application', 'homelab_application')
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND owner_subject_id = (SELECT kombify_ctx.subject())
        )
    );

ALTER POLICY backup_worker_job_read ON jobs
    USING (
        session_user NOT IN ('techstack_backup_worker', 'homelab_backup_worker')
        OR current_user NOT IN ('techstack_backup_worker', 'homelab_backup_worker')
        OR type = 'backup'
    );

ALTER POLICY backup_worker_job_insert ON jobs
    WITH CHECK (
        session_user NOT IN ('techstack_backup_worker', 'homelab_backup_worker')
        OR type = 'backup'
    );

ALTER POLICY backup_worker_job_update ON jobs
    USING (
        session_user NOT IN ('techstack_backup_worker', 'homelab_backup_worker')
        OR type = 'backup'
    )
    WITH CHECK (
        session_user NOT IN ('techstack_backup_worker', 'homelab_backup_worker')
        OR type = 'backup'
    );

ALTER POLICY backup_worker_typed_commands ON typed_agent_commands
    USING (
        session_user NOT IN ('techstack_backup_worker', 'homelab_backup_worker')
        OR (command_family = 'stackkit' AND command_json->>'operation' = 'STACKKIT_OPERATION_BACKUP_RUN')
    )
    WITH CHECK (
        session_user NOT IN ('techstack_backup_worker', 'homelab_backup_worker')
        OR (command_family = 'stackkit' AND command_json->>'operation' = 'STACKKIT_OPERATION_BACKUP_RUN')
    );

ALTER POLICY household_worker_command_ceiling ON typed_agent_commands
    USING (
        session_user NOT IN ('techstack_household_worker', 'homelab_household_worker')
        OR (
            command_family = 'stackkit'
            AND command_json->>'operation' IN (
                'STACKKIT_OPERATION_HOUSEHOLD_PROJECT',
                'STACKKIT_OPERATION_HOUSEHOLD_LIST',
                'STACKKIT_OPERATION_HOUSEHOLD_REMOVE'
            )
        )
    )
    WITH CHECK (
        session_user NOT IN ('techstack_household_worker', 'homelab_household_worker')
        OR (
            command_family = 'stackkit'
            AND command_json->>'operation' IN (
                'STACKKIT_OPERATION_HOUSEHOLD_PROJECT',
                'STACKKIT_OPERATION_HOUSEHOLD_LIST',
                'STACKKIT_OPERATION_HOUSEHOLD_REMOVE'
            )
        )
    );

ALTER POLICY creation_drafts_verified_identity ON creation_drafts
    USING (
        session_user NOT IN ('techstack_application', 'homelab_application')
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND owner_subject_id = (SELECT kombify_ctx.subject())
        )
    )
    WITH CHECK (
        session_user NOT IN ('techstack_application', 'homelab_application')
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND owner_subject_id = (SELECT kombify_ctx.subject())
        )
    );

ALTER POLICY ionos_pool_verified_identity ON ionos_pool_generations
    USING (session_user IN ('techstack_application', 'homelab_application') AND tenant_id = (SELECT kombify_ctx.org_id()))
    WITH CHECK (session_user IN ('techstack_application', 'homelab_application') AND tenant_id = (SELECT kombify_ctx.org_id()));

ALTER POLICY ionos_pool_verified_identity ON ionos_pool_commands
    USING (session_user IN ('techstack_application', 'homelab_application') AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()))
    WITH CHECK (session_user IN ('techstack_application', 'homelab_application') AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()));

ALTER POLICY managed_app_slot_verified_identity ON managed_app_slot_reservations
    USING (session_user IN ('techstack_application', 'homelab_application') AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()))
    WITH CHECK (session_user IN ('techstack_application', 'homelab_application') AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()));

ALTER POLICY fly_machines_verified_identity ON fly_machine_generations
    USING (session_user IN ('techstack_application', 'homelab_application') AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()))
    WITH CHECK (session_user IN ('techstack_application', 'homelab_application') AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()));

ALTER POLICY fly_machines_verified_identity ON fly_machine_commands
    USING (session_user IN ('techstack_application', 'homelab_application') AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()))
    WITH CHECK (session_user IN ('techstack_application', 'homelab_application') AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()));

-- ---------------------------------------------------------------------------
-- 4. Additive homelab_archive schema and role_class aliases
-- ---------------------------------------------------------------------------
-- Note: NOLOGIN pg_roles (homelab_household_worker,
-- homelab_platform_projection_system) and localdb migrationSystemRolesSQL
-- creation are deferred to Phase 2 alongside their GRANTs and TO homelab_*
-- policies so NOCREATEROLE native owners and forward-batch tests continue to
-- succeed in Phase 1 (matching 191_runtime_system_role.sql).

CREATE SCHEMA IF NOT EXISTS homelab_archive;

INSERT INTO kombify_ctx.role_class (role_name, class)
VALUES
    ('homelab_application', 'runtime'),
    ('homelab_backup_worker', 'system'),
    ('homelab_runtime_system', 'system'),
    ('homelab_household_worker', 'system'),
    ('homelab_platform_projection_system', 'system')
ON CONFLICT (role_name) DO NOTHING;

DO $homelab_role_class_verify$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM kombify_ctx.role_class WHERE role_name = 'homelab_application' AND class = 'runtime')
       OR NOT EXISTS (SELECT 1 FROM kombify_ctx.role_class WHERE role_name = 'homelab_backup_worker' AND class = 'system')
       OR NOT EXISTS (SELECT 1 FROM kombify_ctx.role_class WHERE role_name = 'homelab_runtime_system' AND class = 'system')
       OR NOT EXISTS (SELECT 1 FROM kombify_ctx.role_class WHERE role_name = 'homelab_household_worker' AND class = 'system')
       OR NOT EXISTS (SELECT 1 FROM kombify_ctx.role_class WHERE role_name = 'homelab_platform_projection_system' AND class = 'system') THEN
        RAISE EXCEPTION 'homelab role_class registration mismatch';
    END IF;
END
$homelab_role_class_verify$;
