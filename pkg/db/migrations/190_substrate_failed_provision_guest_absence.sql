-- Proxmox substrate provision that failed before binding (kombify-StackKits-2w8s).
--
-- Migration 189 lets a customer-substrate provision that is still pending
-- (requested/accepted, never dispatched) terminalize through resource-free
-- teardown. A substrate provision that instead FAILED before any guest was
-- bound (status failed, phase failed, for example an unavailable template)
-- still could not terminalize: the insert validator of migrations 035, 081 and
-- 098 admits a failed head only with an at-most-once 'no_candidate_observed'
-- operator decision, the substrate provision's provider_correlation dispatch
-- guard counted as unsettled custody forever, and Operator Reconcile refuses a
-- provider_correlation provision. A generation-bound decommission cannot
-- replace it either, because it needs a bound resource.
--
-- A failed substrate provision is not proof that no guest exists: the adapter
-- may have started creating the reserved guest id. The authority added here is
-- the definitive Proxmox absence observation of that exact reserved guest
-- (migrations 041 and 186), recorded by the control plane after the provision
-- failed:
--   * provider 'proxmox', observation 'absent', source 'provider_api';
--   * the guest binding of this resource generation ('guest-<generation>');
--   * the native ref '<substrate_server_id>/<guest_id>' of the provision's own
--     substrate_guest_leases row, both as native_ref_hash and as the
--     authoritative_list_absent entry of the stored document;
--   * recorded under an operation of the same lease and resource generation;
--   * collected after the failed head receipt was issued.
--
-- Changes, all widening for exactly that shape and failing closed otherwise:
--   * provider_operation_resource_free_terminalizations gains the authority
--     'substrate_guest_absent' and absence_evidence_ref, a foreign key to the
--     immutable observation it rests on.
--   * provider_resource_free_terminalization_validate_insert admits a failed
--     substrate head under that authority only with the observation, exact
--     guest custody, no capacity reservation, no live execution claim and no
--     operator resolution state. Pending, commercial and AMO paths are
--     unchanged.
--   * provider_control_count_unsettled_generation_dispatch_guards treats the
--     dispatch guard of such a proven failed provision as settled.
--   * provider_resource_free_terminalization_validate_commit (migration 189)
--     admits the new authority only through exact substrate guest custody.
-- Every row an older binary writes keeps its shape and stays admitted.

SET LOCAL lock_timeout = '5s';

ALTER TABLE provider_operation_resource_free_terminalizations
    ADD COLUMN absence_evidence_ref text,
    ADD CONSTRAINT resource_free_terminalization_absence_evidence_fk
        FOREIGN KEY (tenant_id, absence_evidence_ref)
        REFERENCES provider_absence_observations (tenant_id, evidence_ref)
        ON UPDATE RESTRICT ON DELETE RESTRICT;

-- The authority value check and the authority/evidence shape check of
-- migration 035 carry generated names. Both are located by catalog and
-- replaced by one named shape check.
DO $substrate_absence_authority_checks$
DECLARE
    authority_check record;
    check_count integer := 0;
BEGIN
    FOR authority_check IN
        SELECT constraint_row.conname
        FROM pg_catalog.pg_constraint AS constraint_row
        WHERE constraint_row.conrelid =
                'provider_operation_resource_free_terminalizations'::regclass
          AND constraint_row.contype = 'c'
          AND pg_catalog.pg_get_constraintdef(constraint_row.oid)
                LIKE '%no_dispatch_custody%'
    LOOP
        check_count := check_count + 1;
        EXECUTE pg_catalog.format(
            'ALTER TABLE provider_operation_resource_free_terminalizations DROP CONSTRAINT %I',
            authority_check.conname
        );
    END LOOP;
    IF check_count IS DISTINCT FROM 2 THEN
        RAISE EXCEPTION 'resource-free terminalization authority checks are not at the expected version (% found)', check_count;
    END IF;
END;
$substrate_absence_authority_checks$;

ALTER TABLE provider_operation_resource_free_terminalizations
    ADD CONSTRAINT resource_free_terminalization_authority_shape CHECK (
        (
            authority = 'no_dispatch_custody'
            AND resolution_revision IS NULL
            AND decision_digest IS NULL
            AND absence_evidence_ref IS NULL
        )
        OR (
            authority = 'no_candidate_observed'
            AND resolution_revision IS NOT NULL
            AND decision_digest IS NOT NULL
            AND absence_evidence_ref IS NULL
        )
        OR (
            authority = 'substrate_guest_absent'
            AND resolution_revision IS NULL
            AND decision_digest IS NULL
            AND absence_evidence_ref IS NOT NULL
        )
    );

-- The one predicate every fence below shares. A NULL evidence reference asks
-- whether any qualifying observation exists. It is invoked only from the
-- SECURITY DEFINER fences, runs with their rights, and is not a runtime
-- boundary itself.
CREATE FUNCTION substrate_failed_provision_guest_absent(
    p_tenant text,
    p_operation text,
    p_evidence_ref text
)
RETURNS boolean
LANGUAGE sql
STABLE
SET search_path FROM CURRENT
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM provider_operations AS operation
        JOIN substrate_guest_leases AS guest
          ON guest.tenant_id = operation.tenant_id
         AND guest.lease_id = operation.lease_id
         AND guest.operation_id = operation.operation_id
         AND guest.resource_generation_id::text =
             operation.command_json #>> '{command,resource_generation_id}'
         AND guest.server_id = operation.command_json #>> '{command,runtime_server_id}'
        JOIN provider_operation_receipts AS failed_receipt
          ON failed_receipt.tenant_id = operation.tenant_id
         AND failed_receipt.operation_id = operation.operation_id
         AND failed_receipt.sequence = operation.head_sequence
         AND failed_receipt.receipt_digest = operation.head_receipt_digest
        JOIN provider_operations AS observer
          ON observer.tenant_id = operation.tenant_id
         AND observer.lease_id = operation.lease_id
         AND observer.command_json #>> '{command,resource_generation_id}' =
             operation.command_json #>> '{command,resource_generation_id}'
        JOIN provider_absence_observations AS observation
          ON observation.tenant_id = observer.tenant_id
         AND observation.operation_id = observer.operation_id
        WHERE operation.tenant_id = p_tenant
          AND operation.operation_id = p_operation
          AND operation.operation = 'provision'
          AND operation.status = 'failed'
          AND operation.phase = 'failed'
          AND operation.command_json #>> '{command,provider_id}' = 'proxmox'
          AND failed_receipt.status = 'failed'
          AND failed_receipt.phase = 'failed'
          AND NOT EXISTS (
              SELECT 1
              FROM provider_operation_resources AS resource
              WHERE resource.tenant_id = operation.tenant_id
                AND resource.operation_id = operation.operation_id
          )
          AND (p_evidence_ref IS NULL OR observation.evidence_ref = p_evidence_ref)
          AND observation.provider_id = 'proxmox'
          AND observation.observation = 'absent'
          AND observation.source = 'provider_api'
          AND observation.binding_id = 'guest-' || guest.resource_generation_id::text
          AND observation.native_ref_hash = 'sha256:' || pg_catalog.encode(
              pg_catalog.sha256(pg_catalog.convert_to(
                  guest.substrate_server_id || '/' || guest.guest_id::text, 'UTF8'
              )),
              'hex'
          )
          AND observation.document_json @> pg_catalog.jsonb_build_array(
              pg_catalog.jsonb_build_array(
                  'authoritative_list_absent',
                  guest.substrate_server_id || '/' || guest.guest_id::text
              )
          )
          AND observation.collected_at > failed_receipt.issued_at
    );
$$;

-- Insert validator: four exact patches on the current definition of
-- migrations 035, 081 and 098. Each target must occur exactly once.
DO $substrate_absence_insert_validator$
DECLARE
    function_definition text;
    patch record;
BEGIN
    SELECT pg_get_functiondef(
        'provider_resource_free_terminalization_validate_insert()'::regprocedure
    ) INTO function_definition;
    IF function_definition IS NULL
       OR function_definition LIKE '%substrate_guest_absent%' THEN
        RAISE EXCEPTION 'resource-free terminalization insert validator is not at the expected version';
    END IF;

    FOR patch IN
        SELECT *
        FROM (VALUES
            (
                'failed-head admission',
                $target$            OR (
                operation_status = 'failed'
                AND operation_phase = 'failed'
                AND NEW.authority = 'no_candidate_observed'
            )
       )$target$,
                $replacement$            OR (
                operation_status = 'failed'
                AND operation_phase = 'failed'
                AND NEW.authority = 'no_candidate_observed'
            )
            OR (
                operation_status = 'failed'
                AND operation_phase = 'failed'
                AND NEW.authority = 'substrate_guest_absent'
            )
       )$replacement$
            ),
            (
                'dispatch-guard settlement',
                $target$          AND (operation.command_json #>> '{command,resource_generation_id}')::uuid =
                NEW.resource_generation_id
          AND NOT EXISTS (
              SELECT 1
              FROM provider_provision_resolution_decisions AS decision
              JOIN provider_provision_discovery_observations AS observation$target$,
                $replacement$          AND (operation.command_json #>> '{command,resource_generation_id}')::uuid =
                NEW.resource_generation_id
          AND NOT (
              NEW.authority = 'substrate_guest_absent'
              AND dispatch_guard.operation_id = NEW.operation_id
              AND substrate_failed_provision_guest_absent(
                  NEW.tenant_id, NEW.operation_id, NEW.absence_evidence_ref
              )
          )
          AND NOT EXISTS (
              SELECT 1
              FROM provider_provision_resolution_decisions AS decision
              JOIN provider_provision_discovery_observations AS observation$replacement$
            ),
            (
                'failed-provision settlement',
                $target$          AND NOT (
              operation.operation_id = NEW.operation_id
              AND operation.head_sequence = NEW.head_sequence
              AND operation.head_receipt_digest = NEW.head_receipt_digest
              AND EXISTS ($target$,
                $replacement$          AND NOT (
              NEW.authority = 'substrate_guest_absent'
              AND operation.operation_id = NEW.operation_id
              AND operation.head_sequence = NEW.head_sequence
              AND operation.head_receipt_digest = NEW.head_receipt_digest
              AND substrate_failed_provision_guest_absent(
                  NEW.tenant_id, NEW.operation_id, NEW.absence_evidence_ref
              )
          )
          AND NOT (
              operation.operation_id = NEW.operation_id
              AND operation.head_sequence = NEW.head_sequence
              AND operation.head_receipt_digest = NEW.head_receipt_digest
              AND EXISTS ($replacement$
            ),
            (
                'substrate guest absence authority',
                $target$    ELSE
        PERFORM 1
        FROM provider_provision_resolution_decisions AS decision$target$,
                $replacement$    ELSIF NEW.authority = 'substrate_guest_absent' THEN
        IF NOT substrate_failed_provision_guest_absent(
               NEW.tenant_id, NEW.operation_id, NEW.absence_evidence_ref
           )
           OR NOT EXISTS (
               SELECT 1
               FROM substrate_guest_leases AS guest
               WHERE guest.tenant_id = NEW.tenant_id
                 AND guest.lease_id = NEW.lease_id
                 AND guest.server_id = NEW.server_id
                 AND guest.resource_generation_id = NEW.resource_generation_id
                 AND guest.operation_id = NEW.operation_id
           )
           OR NOT substrate_guest_custody_valid(
               NEW.tenant_id,
               NEW.lease_id,
               NEW.resource_generation_id::text,
               NEW.operation_id
           )
           OR EXISTS (
               SELECT 1
               FROM managed_runtime_capacity_reservations AS reservation
               WHERE reservation.tenant_id = NEW.tenant_id
                 AND reservation.lease_id = NEW.lease_id
                 AND reservation.resource_generation_id = NEW.resource_generation_id
           )
           OR EXISTS (
               SELECT 1
               FROM provider_operation_execution_claims AS claim
               WHERE claim.tenant_id = NEW.tenant_id
                 AND claim.operation_id = NEW.operation_id
                 AND claim.state = 'active'
                 AND claim.lease_expires_at > clock_timestamp()
           )
           OR EXISTS (
               SELECT 1
               FROM provider_provision_discovery_observations AS observation
               WHERE observation.tenant_id = NEW.tenant_id
                 AND observation.operation_id = NEW.operation_id
           )
           OR EXISTS (
               SELECT 1
               FROM provider_provision_resolution_decisions AS decision
               WHERE decision.tenant_id = NEW.tenant_id
                 AND decision.operation_id = NEW.operation_id
           ) THEN
            RAISE EXCEPTION 'substrate guest absence terminalization lacks a definitive absence observation of its exact quiescent reserved guest'
                USING ERRCODE = '55000';
        END IF;
    ELSE
        PERFORM 1
        FROM provider_provision_resolution_decisions AS decision$replacement$
            )
        ) AS patches(name, target, replacement)
    LOOP
        IF pg_catalog.strpos(function_definition, patch.target) = 0
           OR pg_catalog.strpos(
                pg_catalog.substr(
                    function_definition,
                    pg_catalog.strpos(function_definition, patch.target) + 1
                ),
                patch.target
           ) <> 0 THEN
            RAISE EXCEPTION 'resource-free terminalization % patch target is not at the expected version', patch.name;
        END IF;
        function_definition := pg_catalog.replace(
            function_definition, patch.target, patch.replacement
        );
    END LOOP;
    EXECUTE function_definition;
END;
$substrate_absence_insert_validator$;

CREATE OR REPLACE FUNCTION provider_control_count_unsettled_generation_dispatch_guards(
    requested_lease_id text,
    requested_resource_generation_id uuid
)
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path FROM CURRENT
AS $$
DECLARE
    scoped_tenant_id text := NULLIF(current_setting('app.tenant_id', true), '');
    unsettled_count bigint;
BEGIN
    IF scoped_tenant_id IS NULL
       OR requested_lease_id IS NULL
       OR BTRIM(requested_lease_id) = ''
       OR requested_resource_generation_id IS NULL THEN
        RAISE EXCEPTION
            'provider-control dispatch settlement count requires exact tenant, lease, and resource generation'
            USING ERRCODE = '42501';
    END IF;

    SELECT count(*)
    INTO unsettled_count
    FROM provider_provision_dispatch_guards AS dispatch_guard
    JOIN provider_operations AS operation
      ON operation.tenant_id = dispatch_guard.tenant_id
     AND operation.operation_id = dispatch_guard.operation_id
    WHERE operation.tenant_id = scoped_tenant_id
      AND operation.lease_id = requested_lease_id
      AND operation.command_json #>> '{command,resource_generation_id}' =
          requested_resource_generation_id::text
      AND NOT EXISTS (
          SELECT 1
          FROM provider_provision_resolution_decisions AS decision
          JOIN provider_provision_discovery_observations AS observation
            ON observation.tenant_id = decision.tenant_id
           AND observation.operation_id = decision.operation_id
           AND observation.observation_id = decision.observation_id
           AND observation.snapshot_digest =
               decision.observation_snapshot_digest
          WHERE decision.tenant_id = dispatch_guard.tenant_id
            AND decision.operation_id = dispatch_guard.operation_id
            AND decision.outcome = 'no_candidate_observed'
            AND observation.head_sequence = dispatch_guard.head_sequence
            AND observation.head_receipt_digest =
                dispatch_guard.head_receipt_digest
      )
      -- A failed customer-substrate provision is settled once its reserved
      -- guest is definitively observed absent after the failure.
      AND NOT substrate_failed_provision_guest_absent(
          dispatch_guard.tenant_id,
          dispatch_guard.operation_id,
          NULL
      );

    RETURN unsettled_count;
END;
$$;

CREATE OR REPLACE FUNCTION provider_resource_free_terminalization_validate_commit()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path FROM CURRENT
AS $$
BEGIN
    PERFORM 1
    FROM servers AS server
    WHERE server.tenant_id = NEW.tenant_id
      AND server.id = NEW.server_id
      AND server.lease_id = NEW.lease_id
      AND server.generation = NEW.server_generation
      AND server.desired_state = 'absent'
      AND server.lifecycle_state = 'decommissioned'
      AND server.decommissioned_at IS NOT NULL
      AND (
          (
              NEW.authority IN ('no_dispatch_custody', 'no_candidate_observed')
              AND EXISTS (
                  SELECT 1
                  FROM managed_runtime_capacity_release_facts AS release
                  WHERE release.tenant_id = NEW.tenant_id
                    AND release.lease_id = NEW.lease_id
                    AND release.resource_generation_id = NEW.resource_generation_id
                    AND release.server_id = NEW.server_id
                    AND release.server_generation = NEW.server_generation
                    AND release.release_operation_id = NEW.operation_id
                    AND release.release_authority = 'resource_free_teardown'
                    AND release.receipt_sequence = NEW.head_sequence
                    AND release.receipt_digest = NEW.head_receipt_digest
              )
          )
          OR (
              NOT EXISTS (
                  SELECT 1
                  FROM managed_runtime_capacity_reservations AS reservation
                  WHERE reservation.tenant_id = NEW.tenant_id
                    AND reservation.lease_id = NEW.lease_id
                    AND reservation.resource_generation_id = NEW.resource_generation_id
              )
              AND EXISTS (
                  SELECT 1
                  FROM substrate_guest_leases AS guest
                  WHERE guest.tenant_id = NEW.tenant_id
                    AND guest.lease_id = NEW.lease_id
                    AND guest.server_id = NEW.server_id
                    AND guest.resource_generation_id = NEW.resource_generation_id
                    AND guest.operation_id = NEW.operation_id
              )
              AND substrate_guest_custody_valid(
                  NEW.tenant_id,
                  NEW.lease_id,
                  NEW.resource_generation_id::text,
                  NEW.operation_id
              )
          )
      );
    IF NOT FOUND THEN
        RAISE EXCEPTION 'resource-free terminalization must atomically include its exact RuntimeServer tombstone and either its capacity release or exact customer substrate guest custody'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

-- Replacing a function keeps its owner and grants but takes the migration
-- transaction's search path. Restore the runtime boot posture of every
-- provider-control security boundary touched here, and keep the shared
-- predicate private to the fences that call it.
DO $substrate_absence_posture$
DECLARE
    active_schema text := pg_catalog.current_schema();
    boundary_function text;
BEGIN
    FOREACH boundary_function IN ARRAY ARRAY[
        'provider_resource_free_terminalization_validate_insert()',
        'provider_resource_free_terminalization_validate_commit()',
        'provider_control_count_unsettled_generation_dispatch_guards(text, uuid)'
    ]
    LOOP
        EXECUTE pg_catalog.format(
            'ALTER FUNCTION %I.%s SECURITY DEFINER',
            active_schema,
            boundary_function
        );
        EXECUTE pg_catalog.format(
            'ALTER FUNCTION %I.%s SET search_path TO pg_catalog, %I, pg_temp',
            active_schema,
            boundary_function,
            active_schema
        );
        EXECUTE pg_catalog.format(
            'REVOKE ALL ON FUNCTION %I.%s FROM PUBLIC',
            active_schema,
            boundary_function
        );
    END LOOP;
    EXECUTE pg_catalog.format(
        'ALTER FUNCTION %I.substrate_failed_provision_guest_absent(text, text, text) SET search_path TO pg_catalog, %I, pg_temp',
        active_schema,
        active_schema
    );
    EXECUTE pg_catalog.format(
        'REVOKE ALL ON FUNCTION %I.substrate_failed_provision_guest_absent(text, text, text) FROM PUBLIC',
        active_schema
    );
END;
$substrate_absence_posture$;

COMMENT ON FUNCTION substrate_failed_provision_guest_absent(text, text, text) IS
    'True when a failed, resource-free Proxmox substrate provision has a definitive absence observation of its exact reserved guest, collected after the failure.';
COMMENT ON FUNCTION provider_resource_free_terminalization_validate_insert() IS
    'Validates exact resource-free terminalization custody, including a fully tombstoned zero-resource terminal provision and a failed substrate provision whose reserved guest is observed absent.';
COMMENT ON FUNCTION provider_resource_free_terminalization_validate_commit() IS
    'Requires a resource-free terminalization to commit with its exact RuntimeServer tombstone and either its resource_free_teardown capacity release or exact customer substrate guest custody without a capacity reservation.';
