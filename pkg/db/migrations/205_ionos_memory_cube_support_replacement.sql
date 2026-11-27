-- 205_ionos_memory_cube_support_replacement.sql
-- Milestone M1.3: Free IONOS Memory Cube S support replacement durable workflow,
-- company-funded temporary capacity reservation, slot continuity support, and
-- premature recovery exclusion.

SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS managed_runtime_support_replacements (
    tenant_id text NOT NULL,
    replacement_id text NOT NULL,
    owner_subject_id text NOT NULL,
    stack_id text NOT NULL,
    runtime_slot_id text NOT NULL,
    source_lease_id text NOT NULL,
    source_server_id text NOT NULL,
    source_resource_generation_id text NOT NULL,
    source_generation_ordinal bigint NOT NULL CHECK (source_generation_ordinal >= 1),
    source_provider text NOT NULL DEFAULT 'ionos' CHECK (source_provider = 'ionos'),
    source_package_id text NOT NULL DEFAULT 'monthly-runtime-standard' CHECK (source_package_id = 'monthly-runtime-standard'),
    source_product_slug text NOT NULL DEFAULT 'ionos-cube-standard-basic' CHECK (btrim(source_product_slug) <> ''),
    source_execution_profile text NOT NULL DEFAULT 'ionos-basic-cube-standard' CHECK (btrim(source_execution_profile) <> ''),
    target_provider text NOT NULL DEFAULT 'ionos' CHECK (target_provider = 'ionos'),
    target_package_id text NOT NULL DEFAULT 'monthly-runtime-standard' CHECK (target_package_id = 'monthly-runtime-standard'),
    target_variant_name text NOT NULL DEFAULT 'Memory Cube S' CHECK (target_variant_name = 'Memory Cube S'),
    target_product_slug text NOT NULL DEFAULT 'ionos-cube-standard-memory' CHECK (target_product_slug = 'ionos-cube-standard-memory'),
    target_execution_profile text NOT NULL DEFAULT 'ionos-memory-cube-s-standard' CHECK (target_execution_profile = 'ionos-memory-cube-s-standard'),
    target_provider_profile text NOT NULL DEFAULT 'ionos-cloudapi-v6-memory-s' CHECK (target_provider_profile = 'ionos-cloudapi-v6-memory-s'),
    no_charge_confirmed boolean NOT NULL DEFAULT true CHECK (no_charge_confirmed = true),
    case_ref text NOT NULL CHECK (btrim(case_ref) <> ''),
    reason text NOT NULL CHECK (btrim(reason) <> ''),
    authorized_by text NOT NULL CHECK (btrim(authorized_by) <> ''),
    authorized_at timestamptz NOT NULL DEFAULT now(),
    evidence_ref text NOT NULL CHECK (btrim(evidence_ref) <> ''),
    customer_accepted_at timestamptz,
    customer_accepted_by text,
    idempotency_key text NOT NULL CHECK (btrim(idempotency_key) <> ''),
    request_digest text NOT NULL CHECK (btrim(request_digest) <> ''),
    destination_lease_id text,
    destination_server_id text,
    destination_resource_generation_id text,
    destination_generation_ordinal bigint CHECK (
        destination_generation_ordinal IS NULL
        OR destination_generation_ordinal > source_generation_ordinal
    ),
    destination_operation_id text,
    destination_writes_observed boolean NOT NULL DEFAULT false,
    cutover_verified_at timestamptz,
    source_teardown_operation_id text,
    rollback_reason text,
    fenced_conflict_reason text,
    checkpoint_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    phase text NOT NULL DEFAULT 'authorized' CHECK (
        phase IN (
            'authorized',
            'accepted',
            'destination_admitted',
            'staging',
            'cutover_in_progress',
            'cutover_verified',
            'source_decommissioning',
            'completed',
            'rolling_back',
            'rolled_back',
            'fenced_conflict',
            'cancelled'
        )
    ),
    settled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, replacement_id),
    UNIQUE (tenant_id, idempotency_key),
    CHECK (btrim(tenant_id) <> ''),
    CHECK (btrim(replacement_id) <> ''),
    CHECK (btrim(owner_subject_id) <> ''),
    CHECK (btrim(stack_id) <> ''),
    CHECK (btrim(runtime_slot_id) <> ''),
    CHECK (btrim(source_lease_id) <> ''),
    CHECK (btrim(source_server_id) <> ''),
    CHECK (btrim(source_resource_generation_id) <> ''),
    CHECK (
        (customer_accepted_at IS NULL AND customer_accepted_by IS NULL)
        OR (customer_accepted_at IS NOT NULL AND customer_accepted_by = owner_subject_id)
    ),
    CHECK (
        destination_lease_id IS NULL
        OR destination_lease_id <> source_lease_id
    ),
    CHECK (
        destination_resource_generation_id IS NULL
        OR destination_resource_generation_id <> source_resource_generation_id
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS managed_runtime_support_replacements_active_source_uidx
    ON managed_runtime_support_replacements (tenant_id, source_lease_id, source_resource_generation_id)
    WHERE phase NOT IN ('rolled_back', 'cancelled');

CREATE UNIQUE INDEX IF NOT EXISTS managed_runtime_support_replacements_dest_lease_uidx
    ON managed_runtime_support_replacements (tenant_id, destination_lease_id)
    WHERE destination_lease_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS managed_runtime_support_replacements_dest_gen_uidx
    ON managed_runtime_support_replacements (tenant_id, destination_lease_id, destination_resource_generation_id)
    WHERE destination_lease_id IS NOT NULL AND destination_resource_generation_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS managed_runtime_support_replacements_phase_idx
    ON managed_runtime_support_replacements (tenant_id, phase, updated_at);

CREATE INDEX IF NOT EXISTS managed_runtime_support_replacements_slot_idx
    ON managed_runtime_support_replacements (tenant_id, runtime_slot_id);

CREATE OR REPLACE FUNCTION managed_runtime_support_replacements_enforce_transition()
RETURNS trigger
LANGUAGE plpgsql
SET search_path FROM CURRENT
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.phase NOT IN ('authorized') THEN
            RAISE EXCEPTION 'managed_runtime_support_replacements must start in authorized phase (got %)', NEW.phase;
        END IF;
        IF NEW.customer_accepted_at IS NOT NULL OR NEW.destination_writes_observed OR NEW.cutover_verified_at IS NOT NULL THEN
            RAISE EXCEPTION 'managed_runtime_support_replacements cannot start accepted, with writes observed, or cutover verified';
        END IF;
        IF NEW.destination_lease_id IS NOT NULL
            OR NEW.destination_server_id IS NOT NULL
            OR NEW.destination_resource_generation_id IS NOT NULL
            OR NEW.destination_generation_ordinal IS NOT NULL
            OR NEW.destination_operation_id IS NOT NULL THEN
            RAISE EXCEPTION 'managed_runtime_support_replacements cannot start with destination binding columns populated';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.replacement_id IS DISTINCT FROM OLD.replacement_id
        OR NEW.owner_subject_id IS DISTINCT FROM OLD.owner_subject_id
        OR NEW.stack_id IS DISTINCT FROM OLD.stack_id
        OR NEW.runtime_slot_id IS DISTINCT FROM OLD.runtime_slot_id
        OR NEW.source_lease_id IS DISTINCT FROM OLD.source_lease_id
        OR NEW.source_server_id IS DISTINCT FROM OLD.source_server_id
        OR NEW.source_resource_generation_id IS DISTINCT FROM OLD.source_resource_generation_id
        OR NEW.source_generation_ordinal IS DISTINCT FROM OLD.source_generation_ordinal
        OR NEW.source_provider IS DISTINCT FROM OLD.source_provider
        OR NEW.source_package_id IS DISTINCT FROM OLD.source_package_id
        OR NEW.source_product_slug IS DISTINCT FROM OLD.source_product_slug
        OR NEW.source_execution_profile IS DISTINCT FROM OLD.source_execution_profile
        OR NEW.target_provider IS DISTINCT FROM OLD.target_provider
        OR NEW.target_package_id IS DISTINCT FROM OLD.target_package_id
        OR NEW.target_variant_name IS DISTINCT FROM OLD.target_variant_name
        OR NEW.target_product_slug IS DISTINCT FROM OLD.target_product_slug
        OR NEW.target_execution_profile IS DISTINCT FROM OLD.target_execution_profile
        OR NEW.target_provider_profile IS DISTINCT FROM OLD.target_provider_profile
        OR NEW.no_charge_confirmed IS DISTINCT FROM OLD.no_charge_confirmed
        OR NEW.case_ref IS DISTINCT FROM OLD.case_ref
        OR NEW.reason IS DISTINCT FROM OLD.reason
        OR NEW.authorized_by IS DISTINCT FROM OLD.authorized_by
        OR NEW.authorized_at IS DISTINCT FROM OLD.authorized_at
        OR NEW.evidence_ref IS DISTINCT FROM OLD.evidence_ref
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.request_digest IS DISTINCT FROM OLD.request_digest THEN
        RAISE EXCEPTION 'managed_runtime_support_replacements authorization intent and source metadata are immutable';
    END IF;

    IF OLD.destination_writes_observed AND NOT NEW.destination_writes_observed THEN
        RAISE EXCEPTION 'managed_runtime_support_replacements.destination_writes_observed is monotonic';
    END IF;

    IF OLD.cutover_verified_at IS NOT NULL AND NEW.cutover_verified_at IS DISTINCT FROM OLD.cutover_verified_at THEN
        RAISE EXCEPTION 'managed_runtime_support_replacements.cutover_verified_at is monotonic';
    END IF;

    IF OLD.customer_accepted_at IS NOT NULL AND (
        NEW.customer_accepted_at IS DISTINCT FROM OLD.customer_accepted_at
        OR NEW.customer_accepted_by IS DISTINCT FROM OLD.customer_accepted_by
    ) THEN
        RAISE EXCEPTION 'managed_runtime_support_replacements.customer_accepted_at is immutable once set';
    END IF;

    IF OLD.phase IN ('completed', 'rolled_back', 'cancelled') THEN
        RAISE EXCEPTION 'managed_runtime_support_replacements terminal phase % is immutable', OLD.phase;
    END IF;

    IF OLD.destination_lease_id IS NOT NULL AND (
        NEW.destination_lease_id IS DISTINCT FROM OLD.destination_lease_id
        OR NEW.destination_server_id IS DISTINCT FROM OLD.destination_server_id
        OR NEW.destination_resource_generation_id IS DISTINCT FROM OLD.destination_resource_generation_id
        OR NEW.destination_generation_ordinal IS DISTINCT FROM OLD.destination_generation_ordinal
        OR NEW.destination_operation_id IS DISTINCT FROM OLD.destination_operation_id
    ) THEN
        RAISE EXCEPTION 'managed_runtime_support_replacements destination binding is immutable once admitted';
    END IF;

    IF NEW.phase <> OLD.phase THEN
        CASE OLD.phase
            WHEN 'authorized' THEN
                IF NEW.phase NOT IN ('accepted', 'rolling_back', 'rolled_back', 'cancelled') THEN
                    RAISE EXCEPTION 'invalid support replacement transition % -> %', OLD.phase, NEW.phase;
                END IF;
            WHEN 'accepted' THEN
                IF NEW.phase NOT IN ('destination_admitted', 'rolling_back', 'rolled_back', 'cancelled') THEN
                    RAISE EXCEPTION 'invalid support replacement transition % -> %', OLD.phase, NEW.phase;
                END IF;
            WHEN 'destination_admitted' THEN
                IF NEW.phase NOT IN ('staging', 'rolling_back', 'rolled_back', 'fenced_conflict') THEN
                    RAISE EXCEPTION 'invalid support replacement transition % -> %', OLD.phase, NEW.phase;
                END IF;
            WHEN 'staging' THEN
                IF NEW.phase NOT IN ('cutover_in_progress', 'rolling_back', 'rolled_back', 'fenced_conflict') THEN
                    RAISE EXCEPTION 'invalid support replacement transition % -> %', OLD.phase, NEW.phase;
                END IF;
            WHEN 'cutover_in_progress' THEN
                IF NEW.phase NOT IN ('cutover_verified', 'rolling_back', 'rolled_back', 'fenced_conflict') THEN
                    RAISE EXCEPTION 'invalid support replacement transition % -> %', OLD.phase, NEW.phase;
                END IF;
            WHEN 'cutover_verified' THEN
                IF NEW.phase NOT IN ('source_decommissioning', 'completed') THEN
                    RAISE EXCEPTION 'invalid support replacement transition % -> %', OLD.phase, NEW.phase;
                END IF;
            WHEN 'source_decommissioning' THEN
                IF NEW.phase NOT IN ('completed', 'fenced_conflict') THEN
                    RAISE EXCEPTION 'invalid support replacement transition % -> %', OLD.phase, NEW.phase;
                END IF;
            WHEN 'rolling_back' THEN
                IF NEW.phase NOT IN ('rolled_back', 'fenced_conflict') THEN
                    RAISE EXCEPTION 'invalid support replacement transition % -> %', OLD.phase, NEW.phase;
                END IF;
            WHEN 'fenced_conflict' THEN
                IF NEW.phase NOT IN ('cutover_in_progress', 'source_decommissioning', 'rolling_back', 'completed', 'rolled_back') THEN
                    RAISE EXCEPTION 'invalid support replacement transition % -> %', OLD.phase, NEW.phase;
                END IF;
                IF NEW.phase = 'rolling_back' AND (NEW.destination_writes_observed OR NEW.cutover_verified_at IS NOT NULL) THEN
                    RAISE EXCEPTION 'cannot reconcile fenced_conflict to rolling_back after destination writes or verified cutover';
                END IF;
            ELSE
                RAISE EXCEPTION 'support replacement terminal phase % cannot transition to %', OLD.phase, NEW.phase;
        END CASE;
    END IF;

    IF NEW.phase IN ('accepted', 'destination_admitted', 'staging', 'cutover_in_progress', 'cutover_verified', 'source_decommissioning', 'completed')
        AND NEW.customer_accepted_at IS NULL THEN
        RAISE EXCEPTION 'support replacement phase % requires customer_accepted_at', NEW.phase;
    END IF;

    IF NEW.phase IN ('destination_admitted', 'staging', 'cutover_in_progress', 'cutover_verified', 'source_decommissioning', 'completed')
        AND (
            btrim(COALESCE(NEW.destination_lease_id, '')) = ''
            OR btrim(COALESCE(NEW.destination_server_id, '')) = ''
            OR btrim(COALESCE(NEW.destination_resource_generation_id, '')) = ''
            OR NEW.destination_generation_ordinal IS NULL
            OR btrim(COALESCE(NEW.destination_operation_id, '')) = ''
        ) THEN
        RAISE EXCEPTION 'support replacement phase % requires complete destination identity and operation binding', NEW.phase;
    END IF;

    IF NEW.phase IN ('rolling_back', 'rolled_back') AND NEW.destination_writes_observed AND OLD.phase <> 'fenced_conflict' THEN
        RAISE EXCEPTION 'cannot roll back support replacement once destination_writes_observed is true without conflict reconciliation';
    END IF;

    IF NEW.phase IN ('cutover_verified', 'source_decommissioning', 'completed') AND NEW.cutover_verified_at IS NULL THEN
        RAISE EXCEPTION 'support replacement phase % requires cutover_verified_at', NEW.phase;
    END IF;

    IF NEW.phase IN ('completed', 'rolled_back', 'cancelled') AND NEW.settled_at IS NULL THEN
        NEW.settled_at := now();
    END IF;

    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION managed_runtime_support_replacements_enforce_transition() FROM PUBLIC;

DROP TRIGGER IF EXISTS managed_runtime_support_replacements_transition_trigger ON managed_runtime_support_replacements;
CREATE TRIGGER managed_runtime_support_replacements_transition_trigger
    BEFORE INSERT OR UPDATE ON managed_runtime_support_replacements
    FOR EACH ROW
    EXECUTE FUNCTION managed_runtime_support_replacements_enforce_transition();

ALTER TABLE managed_runtime_support_replacements ENABLE ROW LEVEL SECURITY;
ALTER TABLE managed_runtime_support_replacements FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS managed_runtime_support_replacements_tenant_isolation ON managed_runtime_support_replacements;
CREATE POLICY managed_runtime_support_replacements_tenant_isolation ON managed_runtime_support_replacements
    FOR ALL
    USING (tenant_id = NULLIF(pg_catalog.current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(pg_catalog.current_setting('app.tenant_id', true), ''));

REVOKE ALL ON managed_runtime_support_replacements FROM PUBLIC;

-- Extend managed_runtime_capacity_reservations to admit closed company-funded
-- support_replacement_temporary reservations backed by an active replacement.
DO $$
DECLARE
    constraint_record record;
BEGIN
    FOR constraint_record IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'managed_runtime_capacity_reservations'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) LIKE '%reservation_origin%'
    LOOP
        EXECUTE format(
            'ALTER TABLE managed_runtime_capacity_reservations DROP CONSTRAINT IF EXISTS %I',
            constraint_record.conname
        );
    END LOOP;
END;
$$;

ALTER TABLE managed_runtime_capacity_reservations
    ADD CONSTRAINT managed_runtime_capacity_reservations_origin_check
    CHECK (reservation_origin IN ('native_admission', 'migration_quarantine', 'support_replacement_temporary'));

ALTER TABLE managed_runtime_capacity_reservations
    ADD CONSTRAINT managed_runtime_capacity_reservations_policy_origin_check
    CHECK (
        (
            reservation_origin = 'native_admission'
            AND reservation_mode IN ('limited', 'unlimited')
            AND policy_source IN (
                'edge_v2_entitlement+signed_budget:cloud.runtime.credits#managed_servers',
                'static_release_manifest:selfhost-oss'
            )
            AND operation_id IS NOT NULL
            AND BTRIM(operation_id) <> ''
            AND admission_idempotency_key IS NOT NULL
            AND BTRIM(admission_idempotency_key) <> ''
        )
        OR (
            reservation_origin = 'migration_quarantine'
            AND reservation_mode = 'quarantine'
            AND policy_source = 'migration_quarantine:managed-runtime-capacity/v1'
        )
        OR (
            reservation_origin = 'support_replacement_temporary'
            AND reservation_mode = 'limited'
            AND capacity_limit = 1
            AND policy_source = 'support_replacement:ionos_memory_cube_s/v1'
            AND operation_id IS NOT NULL
            AND BTRIM(operation_id) <> ''
            AND admission_idempotency_key IS NOT NULL
            AND BTRIM(admission_idempotency_key) <> ''
        )
    );

CREATE OR REPLACE FUNCTION managed_runtime_capacity_reservation_validate_insert()
RETURNS trigger
LANGUAGE plpgsql
SET search_path FROM CURRENT
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
       OR operation_schema_version IS DISTINCT FROM 'techstack.provider-control-operation/v1'
       OR operation_execution_authority IS DISTINCT FROM 'techstack_provider_control'
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
       OR live_execution_authority IS DISTINCT FROM 'techstack_provider_control' THEN
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

CREATE OR REPLACE FUNCTION provider_operation_require_capacity_reservation()
RETURNS trigger
LANGUAGE plpgsql
SET search_path FROM CURRENT
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
       OR NEW.command_json->>'schema_version' IS DISTINCT FROM 'techstack.provider-control-operation/v1'
       OR NEW.command_json->>'execution_authority' IS DISTINCT FROM 'techstack_provider_control' THEN
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

CREATE OR REPLACE FUNCTION managed_runtime_capacity_execution_claim_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path FROM CURRENT
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
    IF operation_schema_version IS DISTINCT FROM 'techstack.provider-control-operation/v1'
       OR operation_execution_authority IS DISTINCT FROM 'techstack_provider_control'
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

-- Protect active support replacement destinations (and fenced sources) from premature
-- NeverEnrolledRecovery reaping while staging, verifying, cutting over, or fenced.
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
SET search_path FROM CURRENT
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
         AND authority.execution_authority = 'techstack_provider_control'
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

ALTER FUNCTION provider_control_list_never_enrolled_runtime_candidates(text, text, integer, integer)
    SET search_path TO pg_catalog, public, pg_temp;
REVOKE ALL ON FUNCTION provider_control_list_never_enrolled_runtime_candidates(text, text, integer, integer)
    FROM PUBLIC;

-- Replace 197's continuity guard so pre-cutover aborted Memory Cube S
-- support replacement destinations (rolled_back/cancelled without cutover)
-- do not poison future Basic Cube S recreates on the same slot.
CREATE OR REPLACE FUNCTION managed_runtime_slot_memory_cube_continuity_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path FROM CURRENT
AS $$
DECLARE
    prior_adapter_id text;
BEGIN
    IF NEW.generation_ordinal <= 1 THEN
        RETURN NEW;
    END IF;

    SELECT provision.adapter_id
    INTO prior_adapter_id
    FROM managed_runtime_server_slot_generations AS prior
    JOIN provider_operations AS provision
      ON provision.tenant_id = prior.tenant_id
     AND provision.operation_id = prior.provision_operation_id
     AND provision.lease_id = prior.lease_id
    WHERE prior.tenant_id = NEW.tenant_id
      AND prior.slot_id = NEW.slot_id
      AND prior.generation_ordinal < NEW.generation_ordinal
      AND provision.operation = 'provision'
      AND NOT EXISTS (
          SELECT 1
          FROM managed_runtime_support_replacements AS sr
          WHERE sr.tenant_id = prior.tenant_id
            AND sr.runtime_slot_id = prior.slot_id
            AND sr.destination_lease_id = prior.lease_id
            AND sr.destination_resource_generation_id = prior.resource_generation_id::text
            AND sr.phase IN ('rolled_back', 'cancelled')
            AND sr.cutover_verified_at IS NULL
      )
    ORDER BY prior.generation_ordinal DESC
    LIMIT 1;

    IF prior_adapter_id = 'ionos-cloudapi-v6-memory-s'
       AND NOT EXISTS (
           SELECT 1 FROM provider_operations AS provision
           WHERE provision.tenant_id = NEW.tenant_id
             AND provision.operation_id = NEW.provision_operation_id
             AND provision.lease_id = NEW.lease_id
             AND provision.operation = 'provision'
             AND provision.adapter_id = 'ionos-cloudapi-v6-memory-s'
       ) THEN
        RAISE EXCEPTION 'managed runtime recreate requires its prior IONOS Memory Cube S execution profile'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'techstack_provider_control_runtime') THEN
        GRANT SELECT, INSERT, UPDATE ON managed_runtime_support_replacements TO techstack_provider_control_runtime;
        GRANT EXECUTE ON FUNCTION provider_control_list_never_enrolled_runtime_candidates(text, text, integer, integer) TO techstack_provider_control_runtime;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'techstack_runtime_system') THEN
        GRANT SELECT ON managed_runtime_support_replacements TO techstack_runtime_system;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'techstack_application') THEN
        GRANT SELECT ON managed_runtime_support_replacements TO techstack_application;
    END IF;
END;
$$;

-- P1-7: Enforce database-level decommission fence on any lease currently bound
-- by an active or fenced_conflict support replacement.
CREATE OR REPLACE FUNCTION provider_operation_support_replacement_fence_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path FROM CURRENT
AS $$
DECLARE
    sr managed_runtime_support_replacements%ROWTYPE;
BEGIN
    IF NEW.operation IS DISTINCT FROM 'decommission' THEN
        RETURN NEW;
    END IF;

    SELECT *
    INTO sr
    FROM managed_runtime_support_replacements AS r
    WHERE r.tenant_id = NEW.tenant_id
      AND (r.source_lease_id = NEW.lease_id OR r.destination_lease_id = NEW.lease_id)
      AND r.phase IN (
          'destination_admitted',
          'staging',
          'cutover_in_progress',
          'cutover_verified',
          'source_decommissioning',
          'rolling_back',
          'fenced_conflict'
      )
    LIMIT 1;

    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    IF sr.phase = 'rolling_back'
       AND NEW.lease_id = sr.destination_lease_id
       AND NOT sr.destination_writes_observed THEN
        RETURN NEW;
    END IF;

    IF sr.phase IN ('cutover_verified', 'source_decommissioning')
       AND NEW.lease_id = sr.source_lease_id
       AND sr.cutover_verified_at IS NOT NULL
       AND sr.destination_writes_observed THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'lease % is fenced by active support replacement % in phase %', NEW.lease_id, sr.replacement_id, sr.phase
        USING ERRCODE = '55000';
END;
$$;

REVOKE ALL ON FUNCTION provider_operation_support_replacement_fence_guard() FROM PUBLIC;

DROP TRIGGER IF EXISTS provider_operations_support_replacement_fence_guard ON provider_operations;
CREATE TRIGGER provider_operations_support_replacement_fence_guard
BEFORE INSERT ON provider_operations
FOR EACH ROW EXECUTE FUNCTION provider_operation_support_replacement_fence_guard();

-- P1-7: Exclude active and fenced_conflict support replacement leases from
-- stale capacity recovery inside the tenant-scoped RLS refresh trigger, and
-- refresh candidates whenever a support replacement row changes phase.
CREATE OR REPLACE FUNCTION provider_control_refresh_stale_capacity_recovery_tenant()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path FROM CURRENT
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
     AND authority.execution_authority = 'techstack_provider_control'
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

DO $support_replacement_stale_recovery_posture$
DECLARE
    active_schema text := current_schema();
    migration_role text := current_user;
BEGIN
    EXECUTE pg_catalog.format(
        'ALTER FUNCTION %I.provider_control_refresh_stale_capacity_recovery_tenant() OWNER TO %I',
        active_schema,
        migration_role
    );
    EXECUTE pg_catalog.format(
        'ALTER FUNCTION %I.provider_control_refresh_stale_capacity_recovery_tenant() SECURITY DEFINER',
        active_schema
    );
    EXECUTE pg_catalog.format(
        'ALTER FUNCTION %I.provider_control_refresh_stale_capacity_recovery_tenant() SET search_path TO pg_catalog, %I, pg_temp',
        active_schema,
        active_schema
    );
    EXECUTE pg_catalog.format(
        'REVOKE ALL ON FUNCTION %I.provider_control_refresh_stale_capacity_recovery_tenant() FROM PUBLIC',
        active_schema
    );
END
$support_replacement_stale_recovery_posture$;

DROP TRIGGER IF EXISTS support_replacements_refresh_stale_recovery
    ON managed_runtime_support_replacements;
CREATE TRIGGER support_replacements_refresh_stale_recovery
AFTER INSERT OR UPDATE OR DELETE ON managed_runtime_support_replacements
FOR EACH ROW EXECUTE FUNCTION provider_control_refresh_stale_capacity_recovery_tenant();

-- P1-3: Widen jobs_type_check to admit durable support_ionos_memory_replacement
-- jobs and enforce one active (pending/running) support replacement job per
-- replacement_id per tenant.
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_type_check;
ALTER TABLE jobs
    ADD CONSTRAINT jobs_type_check
    CHECK (type IN (
        'provision',
        'destroy',
        'update',
        'restart',
        'deploy',
        'drift_check',
        'drift_resolve',
        'stackkit_lifecycle',
        'remote_enrollment',
        'backup',
        'workload_relocation',
        'support_ionos_memory_replacement'
    ));

CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_active_support_replacement_uidx
    ON jobs (tenant_id, (payload_json->>'replacement_id'))
    WHERE type = 'support_ionos_memory_replacement'
      AND state IN ('pending', 'running')
      AND COALESCE(payload_json->>'replacement_id', '') <> '';

-- Replacement-scoped backup recipient & node-binding handoff staging table.
-- Preserves source node's active pin/binding in stack_backup_stores until
-- destination writes are observed and cutover commits, so pre-cutover
-- rollbacks leave the source Basic Cube S completely intact.
CREATE TABLE IF NOT EXISTS stack_backup_replacement_handoffs (
    tenant_id text NOT NULL,
    stack_id text NOT NULL,
    replacement_id text NOT NULL,
    destination_lease_id text NOT NULL,
    destination_server_id text NOT NULL,
    destination_resource_generation_id text NOT NULL,
    recipient_owner_public_key text NOT NULL,
    recipient_owner_key_id text NOT NULL,
    recipient_fingerprint text NOT NULL CHECK (recipient_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    recipient_generation bigint NOT NULL CHECK (recipient_generation > 0),
    recipient_site_ref text NOT NULL CHECK (btrim(recipient_site_ref) <> ''),
    recipient_node_ref text NOT NULL CHECK (btrim(recipient_node_ref) <> ''),
    recipient_pinned_at timestamptz NOT NULL DEFAULT now(),
    node_backup_target_ref text,
    node_custody_attestation_ref text,
    node_binding_recorded_at timestamptz,
    status text NOT NULL DEFAULT 'staged' CHECK (status IN ('staged', 'committed', 'rolled_back')),
    committed_at timestamptz,
    rolled_back_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, stack_id, replacement_id),
    FOREIGN KEY (tenant_id, stack_id)
        REFERENCES stack_backup_stores (tenant_id, stack_id)
        ON DELETE CASCADE,
    CHECK (btrim(tenant_id) <> ''),
    CHECK (btrim(stack_id) <> ''),
    CHECK (btrim(replacement_id) <> ''),
    CHECK (btrim(destination_lease_id) <> ''),
    CHECK (btrim(destination_server_id) <> ''),
    CHECK (btrim(destination_resource_generation_id) <> ''),
    CHECK (btrim(recipient_owner_public_key) <> ''),
    CHECK (btrim(recipient_owner_key_id) <> ''),
    CHECK (
        (node_backup_target_ref IS NULL AND node_custody_attestation_ref IS NULL AND node_binding_recorded_at IS NULL)
        OR (
            node_backup_target_ref ~ '^backup-target://sha256/[0-9a-f]{64}$'
            AND node_custody_attestation_ref ~ '^backup-custody-attestation://sha256/[0-9a-f]{64}$'
            AND node_binding_recorded_at IS NOT NULL
        )
    ),
    CHECK (
        (status = 'staged' AND committed_at IS NULL AND rolled_back_at IS NULL)
        OR (status = 'committed' AND committed_at IS NOT NULL AND rolled_back_at IS NULL AND node_backup_target_ref IS NOT NULL)
        OR (status = 'rolled_back' AND rolled_back_at IS NOT NULL AND committed_at IS NULL)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS stack_backup_replacement_handoffs_active_stack_uidx
    ON stack_backup_replacement_handoffs (tenant_id, stack_id)
    WHERE status = 'staged';

CREATE OR REPLACE FUNCTION stack_backup_replacement_handoff_enforce()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    scoped_tenant text := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    sr managed_runtime_support_replacements%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF EXISTS (
            SELECT 1 FROM stack_backup_stores s
            WHERE s.tenant_id = OLD.tenant_id AND s.stack_id = OLD.stack_id
        ) THEN
            RAISE EXCEPTION 'stack backup replacement handoffs cannot be deleted' USING ERRCODE = '55000';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM scoped_tenant THEN
        RAISE EXCEPTION 'stack backup replacement handoff requires exact tenant scope' USING ERRCODE = '42501';
    END IF;

    SELECT *
    INTO sr
    FROM managed_runtime_support_replacements AS r
    WHERE r.tenant_id = NEW.tenant_id
      AND r.stack_id = NEW.stack_id
      AND r.replacement_id = NEW.replacement_id
      AND r.destination_lease_id = NEW.destination_lease_id
      AND r.destination_server_id = NEW.destination_server_id
      AND r.destination_resource_generation_id = NEW.destination_resource_generation_id
      AND r.customer_accepted_at IS NOT NULL
      AND r.no_charge_confirmed = true
      AND r.target_product_slug = 'ionos-cube-standard-memory';

    IF NOT FOUND THEN
        RAISE EXCEPTION 'backup replacement handoff requires customer-accepted Memory Cube S support replacement'
            USING ERRCODE = '55000';
    END IF;

    IF TG_OP = 'UPDATE' THEN
        IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
            OR NEW.stack_id IS DISTINCT FROM OLD.stack_id
            OR NEW.replacement_id IS DISTINCT FROM OLD.replacement_id
            OR NEW.destination_lease_id IS DISTINCT FROM OLD.destination_lease_id
            OR NEW.destination_server_id IS DISTINCT FROM OLD.destination_server_id
            OR NEW.destination_resource_generation_id IS DISTINCT FROM OLD.destination_resource_generation_id
            OR NEW.recipient_owner_public_key IS DISTINCT FROM OLD.recipient_owner_public_key
            OR NEW.recipient_owner_key_id IS DISTINCT FROM OLD.recipient_owner_key_id
            OR NEW.recipient_fingerprint IS DISTINCT FROM OLD.recipient_fingerprint
            OR NEW.recipient_generation IS DISTINCT FROM OLD.recipient_generation
            OR NEW.recipient_site_ref IS DISTINCT FROM OLD.recipient_site_ref
            OR NEW.recipient_node_ref IS DISTINCT FROM OLD.recipient_node_ref THEN
            RAISE EXCEPTION 'staged backup replacement recipient pin is immutable' USING ERRCODE = '55000';
        END IF;
        IF OLD.node_backup_target_ref IS NOT NULL AND (
            NEW.node_backup_target_ref IS DISTINCT FROM OLD.node_backup_target_ref
            OR NEW.node_custody_attestation_ref IS DISTINCT FROM OLD.node_custody_attestation_ref
        ) THEN
            RAISE EXCEPTION 'staged backup replacement node binding is immutable once recorded' USING ERRCODE = '55000';
        END IF;
        IF OLD.status = 'committed' AND NEW.status <> 'committed' THEN
            RAISE EXCEPTION 'committed backup replacement handoff is terminal' USING ERRCODE = '55000';
        END IF;
        IF OLD.status = 'rolled_back' AND NEW.status <> 'rolled_back' THEN
            RAISE EXCEPTION 'rolled_back backup replacement handoff is terminal' USING ERRCODE = '55000';
        END IF;
    END IF;

    IF NEW.status = 'staged' THEN
        IF sr.phase NOT IN ('destination_admitted', 'staging') THEN
            RAISE EXCEPTION 'staging backup replacement handoff requires destination_admitted or staging phase (got %)', sr.phase
                USING ERRCODE = '55000';
        END IF;
    ELSIF NEW.status = 'committed' THEN
        IF NOT sr.destination_writes_observed OR sr.phase NOT IN ('cutover_in_progress', 'cutover_verified', 'completed') THEN
            RAISE EXCEPTION 'committing backup replacement handoff requires observed destination writes during cutover'
                USING ERRCODE = '55000';
        END IF;
    ELSIF NEW.status = 'rolled_back' THEN
        IF sr.phase IN ('cutover_verified', 'source_decommissioning', 'completed') OR sr.cutover_verified_at IS NOT NULL THEN
            RAISE EXCEPTION 'cannot roll back backup replacement handoff after verified cutover'
                USING ERRCODE = '55000';
        END IF;
        IF sr.destination_writes_observed AND sr.phase <> 'rolled_back' THEN
            RAISE EXCEPTION 'cannot roll back backup replacement handoff after destination writes without conflict reconciliation to rolled_back'
                USING ERRCODE = '55000';
        END IF;
    END IF;

    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION stack_backup_replacement_handoff_enforce() FROM PUBLIC;

DROP TRIGGER IF EXISTS stack_backup_replacement_handoff_enforce_trigger ON stack_backup_replacement_handoffs;
CREATE TRIGGER stack_backup_replacement_handoff_enforce_trigger
    BEFORE INSERT OR UPDATE OR DELETE ON stack_backup_replacement_handoffs
    FOR EACH ROW EXECUTE FUNCTION stack_backup_replacement_handoff_enforce();

ALTER TABLE stack_backup_replacement_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE stack_backup_replacement_handoffs FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON stack_backup_replacement_handoffs;
CREATE POLICY tenant_isolation ON stack_backup_replacement_handoffs
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'techstack_application') THEN
        GRANT SELECT, INSERT, UPDATE ON stack_backup_replacement_handoffs TO techstack_application;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'techstack_runtime_system') THEN
        GRANT SELECT, INSERT, UPDATE ON stack_backup_replacement_handoffs TO techstack_runtime_system;
    END IF;
END;
$$;
