-- Never-enrolled managed runtime directory.
--
-- provider_control_list_never_enrolled_runtime_candidates (114, extended by
-- 205 and 206) is the only discovery of managed VMs that were provisioned,
-- billed and never enrolled. It is SECURITY DEFINER, but the migration owner
-- that defines it is NOBYPASSRLS and every table it joins is FORCE ROW LEVEL
-- SECURITY: managed_runtime_capacity_reservations, techstack_vm_leases,
-- runtime_lease_execution_authorities, managed_runtime_capacity_release_facts,
-- managed_runtime_support_replacements, servers and jobs. The provider-control
-- runtime calls it without app.tenant_id, so the definer saw no tenant's rows
-- and the reaper found nothing: never-enrolled VMs stayed billed (B1). 093,
-- 201 and 207 record the same fact for their tables.
--
-- Like 093, 201 and 207, this migration keeps secret-free, migration-owned
-- directories that the source writes maintain while they still carry their
-- tenant scope, and redefines the function to read only those directories.
-- Its signature, result columns, bounds, filters, keyset order and EXECUTE
-- grants stay exactly as 206 left them.
--
--   * provider_never_enrolled_runtime_leases holds, per lease, the lease-level
--     part of the 206 predicate: an unreleased reservation of the lease's
--     current resource generation, provider-control execution authority
--     (techstack_ or homelab_provider_control), an uncancelled lease whose
--     desired state is not absent, a non-empty stack_id and no active support
--     replacement naming the lease as source or destination (205). Every input
--     row is keyed by that one lease, so a write rebuilds only its own lease's
--     entry. The rebuild serializes on the tenant key the stale-capacity
--     directory (093, 163, 205, 206) already takes on exactly these five
--     tables, so it adds no lock that the same writes do not already hold.
--   * provider_never_enrolled_runtime_server_evidence holds one row per
--     server that excludes its lease: enrolled (worker, heartbeat, active or
--     connected/degraded) or being decommissioned. It is derived from the
--     written server row alone, under that row's own lock, takes no advisory
--     lock and writes nothing for a heartbeat that changes neither its key nor
--     its evidence (163).
--   * provider_never_enrolled_runtime_active_jobs holds one row per pending,
--     running or waiting job with a stack, derived from the written job row
--     alone, and writes nothing for a progress or log update.
--
-- The time window (reserved_at) and the three NOT EXISTS joins are evaluated
-- at read time, so the result equals the 206 query over the same rows.
--
-- Every writer of the seven source tables already carries the exact tenant
-- scope: the stale-capacity trigger (093/206) and the provision-wait trigger
-- (084) refuse any write without it. These triggers apply the same check.
-- The provider-control runtime receives EXECUTE on the read and nothing on the
-- directories. The seed reads the source rows inside this migration
-- transaction through owner-only read policies that exist only until it
-- commits (196, 201, 207); the table locks keep every write out between the
-- seed and the triggers.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '2min';
SELECT pg_catalog.set_config(
    'search_path',
    pg_catalog.quote_ident(pg_catalog.current_schema()) || ', pg_catalog, pg_temp',
    true
);

CREATE TABLE IF NOT EXISTS provider_never_enrolled_runtime_leases (
    tenant_id text NOT NULL CHECK (BTRIM(tenant_id) <> ''),
    lease_id text NOT NULL CHECK (BTRIM(lease_id) <> ''),
    owner_subject_id text NOT NULL CHECK (BTRIM(owner_subject_id) <> ''),
    stack_id text NOT NULL CHECK (stack_id <> ''),
    server_id text,
    reserved_at timestamptz NOT NULL,
    refreshed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, lease_id)
);

-- The two row mirrors carry only constraints their source rows already
-- satisfy, so maintaining them never refuses a write the source admits.
CREATE TABLE IF NOT EXISTS provider_never_enrolled_runtime_server_evidence (
    tenant_id text NOT NULL,
    server_id text NOT NULL,
    lease_id text NOT NULL,
    refreshed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, server_id)
);

CREATE TABLE IF NOT EXISTS provider_never_enrolled_runtime_active_jobs (
    tenant_id text NOT NULL,
    job_id text NOT NULL,
    stack_id text NOT NULL CHECK (stack_id <> ''),
    refreshed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, job_id)
);

CREATE INDEX IF NOT EXISTS provider_never_enrolled_runtime_leases_reserved_at_idx
    ON provider_never_enrolled_runtime_leases (reserved_at);
CREATE INDEX IF NOT EXISTS provider_never_enrolled_runtime_active_jobs_stack_idx
    ON provider_never_enrolled_runtime_active_jobs (tenant_id, stack_id);

REVOKE ALL ON TABLE provider_never_enrolled_runtime_leases FROM PUBLIC;
REVOKE ALL ON TABLE provider_never_enrolled_runtime_server_evidence FROM PUBLIC;
REVOKE ALL ON TABLE provider_never_enrolled_runtime_active_jobs FROM PUBLIC;

-- Rebuilds the directory entry of one lease from the rows its definer can
-- see: the scoped tenant's rows at runtime, every row during the seed. A
-- lease of another tenant therefore only loses its entry, never gains one.
-- Not SECURITY DEFINER: only the owner's triggers and seed call it.
CREATE OR REPLACE FUNCTION provider_control_rebuild_never_enrolled_runtime_lease(
    p_tenant_id text,
    p_lease_id text
)
RETURNS void
LANGUAGE plpgsql
SET search_path FROM CURRENT
AS $$
BEGIN
    IF p_tenant_id IS NULL OR BTRIM(p_tenant_id) = ''
       OR p_lease_id IS NULL OR BTRIM(p_lease_id) = '' THEN
        RETURN;
    END IF;

    DELETE FROM provider_never_enrolled_runtime_leases AS entry
    WHERE entry.tenant_id = p_tenant_id
      AND entry.lease_id = p_lease_id;

    INSERT INTO provider_never_enrolled_runtime_leases (
        tenant_id,
        lease_id,
        owner_subject_id,
        stack_id,
        server_id,
        reserved_at,
        refreshed_at
    )
    SELECT
        reservation.tenant_id,
        reservation.lease_id,
        reservation.owner_subject_id,
        lease.lease_json->'metadata'->>'stack_id',
        lease.server_id,
        reservation.reserved_at,
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
    WHERE reservation.tenant_id = p_tenant_id
      AND reservation.lease_id = p_lease_id
      AND NOT EXISTS (
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
      AND COALESCE(lease.lease_json->'metadata'->>'stack_id', '') <> ''
    ON CONFLICT (tenant_id, lease_id) DO UPDATE
    SET owner_subject_id = EXCLUDED.owner_subject_id,
        stack_id = EXCLUDED.stack_id,
        server_id = EXCLUDED.server_id,
        reserved_at = EXCLUDED.reserved_at,
        refreshed_at = EXCLUDED.refreshed_at;
END;
$$;

-- Lease-level sources. TG_ARGV[0] names how the written row names its lease:
-- 'id' (techstack_vm_leases), 'lease_id' (reservations, release facts,
-- execution authorities) or 'support_replacement' (source and destination).
CREATE OR REPLACE FUNCTION provider_control_refresh_never_enrolled_runtime_leases()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path FROM CURRENT
AS $$
DECLARE
    refresh_tenant_id text := COALESCE(NEW.tenant_id, OLD.tenant_id);
    scoped_tenant_id text := NULLIF(current_setting('app.tenant_id', true), '');
    key_mode text := TG_ARGV[0];
    affected_tenants text[] := ARRAY[]::text[];
    affected_leases text[] := ARRAY[]::text[];
    entry_index integer;
BEGIN
    IF refresh_tenant_id IS NULL OR BTRIM(refresh_tenant_id) = '' THEN
        RAISE EXCEPTION 'never-enrolled runtime refresh requires an exact tenant'
            USING ERRCODE = '55000';
    END IF;
    IF scoped_tenant_id IS DISTINCT FROM refresh_tenant_id THEN
        RAISE EXCEPTION 'never-enrolled runtime refresh requires the exact tenant scope'
            USING ERRCODE = '42501';
    END IF;
    IF key_mode IS NULL OR key_mode NOT IN ('id', 'lease_id', 'support_replacement') THEN
        RAISE EXCEPTION 'never-enrolled runtime refresh requires a known lease key'
            USING ERRCODE = '55000';
    END IF;

    -- The same tenant key the stale-capacity directory trigger takes on every
    -- write of these five tables, so this adds no new lock or lock order.
    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(
            'providercontrol.stale-capacity-recovery/v1:' || refresh_tenant_id,
            0
        )
    );

    -- plpgsql resolves a row field only when its statement runs, so each
    -- branch reads only the columns of the table its key mode names.
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        IF key_mode = 'id' THEN
            affected_tenants := affected_tenants || ARRAY[OLD.tenant_id];
            affected_leases := affected_leases || ARRAY[OLD.id];
        ELSIF key_mode = 'lease_id' THEN
            affected_tenants := affected_tenants || ARRAY[OLD.tenant_id];
            affected_leases := affected_leases || ARRAY[OLD.lease_id];
        ELSE
            affected_tenants := affected_tenants || ARRAY[OLD.tenant_id, OLD.tenant_id];
            affected_leases := affected_leases || ARRAY[OLD.source_lease_id, OLD.destination_lease_id];
        END IF;
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        IF key_mode = 'id' THEN
            affected_tenants := affected_tenants || ARRAY[NEW.tenant_id];
            affected_leases := affected_leases || ARRAY[NEW.id];
        ELSIF key_mode = 'lease_id' THEN
            affected_tenants := affected_tenants || ARRAY[NEW.tenant_id];
            affected_leases := affected_leases || ARRAY[NEW.lease_id];
        ELSE
            affected_tenants := affected_tenants || ARRAY[NEW.tenant_id, NEW.tenant_id];
            affected_leases := affected_leases || ARRAY[NEW.source_lease_id, NEW.destination_lease_id];
        END IF;
    END IF;

    FOR entry_index IN 1 .. COALESCE(pg_catalog.array_length(affected_leases, 1), 0) LOOP
        PERFORM provider_control_rebuild_never_enrolled_runtime_lease(
            affected_tenants[entry_index],
            affected_leases[entry_index]
        );
    END LOOP;

    RETURN COALESCE(NEW, OLD);
END;
$$;

-- One evidence row per server whose own row excludes its lease from the
-- never-enrolled candidates (the two servers NOT EXISTS clauses of 206).
CREATE OR REPLACE FUNCTION provider_control_refresh_never_enrolled_runtime_server()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path FROM CURRENT
AS $$
DECLARE
    server_tenant_id text := COALESCE(NEW.tenant_id, OLD.tenant_id);
    scoped_tenant_id text := NULLIF(current_setting('app.tenant_id', true), '');
    old_excludes boolean := false;
    new_excludes boolean := false;
BEGIN
    IF server_tenant_id IS NULL OR BTRIM(server_tenant_id) = '' THEN
        RAISE EXCEPTION 'never-enrolled server evidence refresh requires an exact tenant'
            USING ERRCODE = '55000';
    END IF;
    IF scoped_tenant_id IS DISTINCT FROM server_tenant_id THEN
        RAISE EXCEPTION 'never-enrolled server evidence refresh requires the exact tenant scope'
            USING ERRCODE = '42501';
    END IF;

    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        old_excludes := OLD.lease_id IS NOT NULL AND COALESCE(
            (
                OLD.desired_state = 'absent'
                AND (
                    OLD.lifecycle_state IN ('decommissioning', 'decommissioned')
                    OR OLD.decommissioned_at IS NOT NULL
                )
            )
            OR OLD.worker_id IS NOT NULL
            OR OLD.last_heartbeat_at IS NOT NULL
            OR OLD.lifecycle_state = 'active'
            OR OLD.connection_state IN ('connected', 'degraded'),
            false
        );
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        new_excludes := NEW.lease_id IS NOT NULL AND COALESCE(
            (
                NEW.desired_state = 'absent'
                AND (
                    NEW.lifecycle_state IN ('decommissioning', 'decommissioned')
                    OR NEW.decommissioned_at IS NOT NULL
                )
            )
            OR NEW.worker_id IS NOT NULL
            OR NEW.last_heartbeat_at IS NOT NULL
            OR NEW.lifecycle_state = 'active'
            OR NEW.connection_state IN ('connected', 'degraded'),
            false
        );
    END IF;

    -- A heartbeat or inventory update keeps the key and the evidence: the
    -- entry is already exact, so nothing is written (163).
    IF TG_OP = 'UPDATE' THEN
        IF OLD.tenant_id IS NOT DISTINCT FROM NEW.tenant_id
           AND OLD.id IS NOT DISTINCT FROM NEW.id
           AND OLD.lease_id IS NOT DISTINCT FROM NEW.lease_id
           AND old_excludes = new_excludes THEN
            RETURN NEW;
        END IF;
    END IF;

    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        DELETE FROM provider_never_enrolled_runtime_server_evidence AS evidence
        WHERE evidence.tenant_id = OLD.tenant_id
          AND evidence.server_id = OLD.id;
    END IF;
    IF new_excludes THEN
        INSERT INTO provider_never_enrolled_runtime_server_evidence (
            tenant_id,
            server_id,
            lease_id,
            refreshed_at
        ) VALUES (
            NEW.tenant_id,
            NEW.id,
            NEW.lease_id,
            clock_timestamp()
        )
        ON CONFLICT (tenant_id, server_id) DO UPDATE
        SET lease_id = EXCLUDED.lease_id,
            refreshed_at = EXCLUDED.refreshed_at;
    END IF;

    RETURN COALESCE(NEW, OLD);
END;
$$;

-- One row per job that keeps the reaper off its stack (206: pending,
-- running or waiting).
CREATE OR REPLACE FUNCTION provider_control_refresh_never_enrolled_runtime_job()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path FROM CURRENT
AS $$
DECLARE
    job_tenant_id text := COALESCE(NEW.tenant_id, OLD.tenant_id);
    scoped_tenant_id text := NULLIF(current_setting('app.tenant_id', true), '');
    old_active boolean := false;
    new_active boolean := false;
BEGIN
    IF job_tenant_id IS NULL OR BTRIM(job_tenant_id) = '' THEN
        RAISE EXCEPTION 'never-enrolled active job refresh requires an exact tenant'
            USING ERRCODE = '55000';
    END IF;
    IF scoped_tenant_id IS DISTINCT FROM job_tenant_id THEN
        RAISE EXCEPTION 'never-enrolled active job refresh requires the exact tenant scope'
            USING ERRCODE = '42501';
    END IF;

    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        old_active := COALESCE(
            OLD.stack_id <> '' AND OLD.state IN ('pending', 'running', 'waiting'),
            false
        );
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        new_active := COALESCE(
            NEW.stack_id <> '' AND NEW.state IN ('pending', 'running', 'waiting'),
            false
        );
    END IF;

    -- Progress, step, log and result updates keep the entry exact.
    IF TG_OP = 'UPDATE' THEN
        IF OLD.tenant_id IS NOT DISTINCT FROM NEW.tenant_id
           AND OLD.id IS NOT DISTINCT FROM NEW.id
           AND OLD.stack_id IS NOT DISTINCT FROM NEW.stack_id
           AND old_active = new_active THEN
            RETURN NEW;
        END IF;
    END IF;

    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        DELETE FROM provider_never_enrolled_runtime_active_jobs AS active_job
        WHERE active_job.tenant_id = OLD.tenant_id
          AND active_job.job_id = OLD.id;
    END IF;
    IF new_active THEN
        INSERT INTO provider_never_enrolled_runtime_active_jobs (
            tenant_id,
            job_id,
            stack_id,
            refreshed_at
        ) VALUES (
            NEW.tenant_id,
            NEW.id,
            NEW.stack_id,
            clock_timestamp()
        )
        ON CONFLICT (tenant_id, job_id) DO UPDATE
        SET stack_id = EXCLUDED.stack_id,
            refreshed_at = EXCLUDED.refreshed_at;
    END IF;

    RETURN COALESCE(NEW, OLD);
END;
$$;

-- The 206 read, over the directories. Same signature, bounds, columns,
-- filters and keyset order; SECURITY DEFINER only to read the directories.
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
    SELECT
        candidate.tenant_id,
        candidate.owner_subject_id,
        candidate.lease_id,
        candidate.stack_id,
        candidate.reserved_at
    FROM provider_never_enrolled_runtime_leases AS candidate
    WHERE candidate.reserved_at < clock_timestamp() - make_interval(secs => enrollment_window_seconds)
      AND NOT EXISTS (
              SELECT 1
              FROM provider_never_enrolled_runtime_server_evidence AS evidence
              WHERE evidence.tenant_id = candidate.tenant_id
                AND evidence.server_id = candidate.server_id
                AND evidence.lease_id = candidate.lease_id
          )
      AND NOT EXISTS (
              SELECT 1
              FROM provider_never_enrolled_runtime_active_jobs AS active_job
              WHERE active_job.tenant_id = candidate.tenant_id
                AND active_job.stack_id = candidate.stack_id
          )
      AND (candidate.tenant_id, candidate.lease_id) > (
          COALESCE(BTRIM(after_tenant_id), ''),
          COALESCE(BTRIM(after_lease_id), '')
      )
    ORDER BY candidate.tenant_id, candidate.lease_id
    LIMIT requested_limit;
END;
$$;

DO $never_enrolled_runtime_owner$
DECLARE
    source_table text;
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname = current_user AND (rolsuper OR rolbypassrls)
    ) THEN
        RAISE EXCEPTION 'never-enrolled runtime directory refuses a superuser or BYPASSRLS migration owner'
            USING ERRCODE = '42501';
    END IF;
    FOREACH source_table IN ARRAY ARRAY[
        'managed_runtime_capacity_reservations',
        'managed_runtime_capacity_release_facts',
        'techstack_vm_leases',
        'runtime_lease_execution_authorities',
        'managed_runtime_support_replacements',
        'servers',
        'jobs'
    ] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_class AS source_relation
            WHERE source_relation.oid = pg_catalog.to_regclass(source_table)
              AND source_relation.relowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
              AND source_relation.relrowsecurity
              AND source_relation.relforcerowsecurity
        ) THEN
            RAISE EXCEPTION 'never-enrolled runtime directory requires the migration owner and FORCE RLS on %', source_table
                USING ERRCODE = '42501';
        END IF;
    END LOOP;
END
$never_enrolled_runtime_owner$;

-- The locks block every source write until commit, so no write falls between
-- the seed and the triggers.
LOCK TABLE managed_runtime_capacity_reservations,
    managed_runtime_capacity_release_facts,
    techstack_vm_leases,
    runtime_lease_execution_authorities,
    managed_runtime_support_replacements,
    servers,
    jobs
    IN SHARE ROW EXCLUSIVE MODE;

DROP TRIGGER IF EXISTS managed_capacity_reservations_refresh_never_enrolled
    ON managed_runtime_capacity_reservations;
CREATE TRIGGER managed_capacity_reservations_refresh_never_enrolled
AFTER INSERT OR UPDATE OR DELETE ON managed_runtime_capacity_reservations
FOR EACH ROW EXECUTE FUNCTION provider_control_refresh_never_enrolled_runtime_leases('lease_id');

DROP TRIGGER IF EXISTS managed_capacity_releases_refresh_never_enrolled
    ON managed_runtime_capacity_release_facts;
CREATE TRIGGER managed_capacity_releases_refresh_never_enrolled
AFTER INSERT OR UPDATE OR DELETE ON managed_runtime_capacity_release_facts
FOR EACH ROW EXECUTE FUNCTION provider_control_refresh_never_enrolled_runtime_leases('lease_id');

DROP TRIGGER IF EXISTS runtime_leases_refresh_never_enrolled
    ON techstack_vm_leases;
CREATE TRIGGER runtime_leases_refresh_never_enrolled
AFTER INSERT OR UPDATE OR DELETE ON techstack_vm_leases
FOR EACH ROW EXECUTE FUNCTION provider_control_refresh_never_enrolled_runtime_leases('id');

DROP TRIGGER IF EXISTS lease_authorities_refresh_never_enrolled
    ON runtime_lease_execution_authorities;
CREATE TRIGGER lease_authorities_refresh_never_enrolled
AFTER INSERT OR UPDATE OR DELETE ON runtime_lease_execution_authorities
FOR EACH ROW EXECUTE FUNCTION provider_control_refresh_never_enrolled_runtime_leases('lease_id');

DROP TRIGGER IF EXISTS support_replacements_refresh_never_enrolled
    ON managed_runtime_support_replacements;
CREATE TRIGGER support_replacements_refresh_never_enrolled
AFTER INSERT OR UPDATE OR DELETE ON managed_runtime_support_replacements
FOR EACH ROW EXECUTE FUNCTION provider_control_refresh_never_enrolled_runtime_leases('support_replacement');

DROP TRIGGER IF EXISTS servers_refresh_never_enrolled_evidence
    ON servers;
CREATE TRIGGER servers_refresh_never_enrolled_evidence
AFTER INSERT OR UPDATE OR DELETE ON servers
FOR EACH ROW EXECUTE FUNCTION provider_control_refresh_never_enrolled_runtime_server();

DROP TRIGGER IF EXISTS jobs_refresh_never_enrolled_active_jobs
    ON jobs;
CREATE TRIGGER jobs_refresh_never_enrolled_active_jobs
AFTER INSERT OR UPDATE OR DELETE ON jobs
FOR EACH ROW EXECUTE FUNCTION provider_control_refresh_never_enrolled_runtime_job();

-- Seed the three directories from the live rows. The owner-only read policies
-- exist only inside this migration transaction.
DO $never_enrolled_runtime_seed$
DECLARE
    active_schema text := pg_catalog.current_schema();
    source_table text;
    seed_lease record;
BEGIN
    FOREACH source_table IN ARRAY ARRAY[
        'managed_runtime_capacity_reservations',
        'managed_runtime_capacity_release_facts',
        'techstack_vm_leases',
        'runtime_lease_execution_authorities',
        'managed_runtime_support_replacements',
        'servers',
        'jobs'
    ] LOOP
        EXECUTE pg_catalog.format(
            'CREATE POLICY never_enrolled_runtime_seed ON %I.%I FOR SELECT TO %I USING (true)',
            active_schema, source_table, current_user
        );
    END LOOP;

    FOR seed_lease IN
        SELECT DISTINCT reservation.tenant_id, reservation.lease_id
        FROM managed_runtime_capacity_reservations AS reservation
        ORDER BY reservation.tenant_id, reservation.lease_id
    LOOP
        PERFORM provider_control_rebuild_never_enrolled_runtime_lease(
            seed_lease.tenant_id,
            seed_lease.lease_id
        );
    END LOOP;

    INSERT INTO provider_never_enrolled_runtime_server_evidence (
        tenant_id,
        server_id,
        lease_id,
        refreshed_at
    )
    SELECT server.tenant_id, server.id, server.lease_id, clock_timestamp()
    FROM servers AS server
    WHERE server.lease_id IS NOT NULL
      AND COALESCE(
          (
              server.desired_state = 'absent'
              AND (
                  server.lifecycle_state IN ('decommissioning', 'decommissioned')
                  OR server.decommissioned_at IS NOT NULL
              )
          )
          OR server.worker_id IS NOT NULL
          OR server.last_heartbeat_at IS NOT NULL
          OR server.lifecycle_state = 'active'
          OR server.connection_state IN ('connected', 'degraded'),
          false
      )
    ON CONFLICT (tenant_id, server_id) DO UPDATE
    SET lease_id = EXCLUDED.lease_id,
        refreshed_at = EXCLUDED.refreshed_at;

    INSERT INTO provider_never_enrolled_runtime_active_jobs (
        tenant_id,
        job_id,
        stack_id,
        refreshed_at
    )
    SELECT job.tenant_id, job.id, job.stack_id, clock_timestamp()
    FROM jobs AS job
    WHERE COALESCE(
          job.stack_id <> '' AND job.state IN ('pending', 'running', 'waiting'),
          false
      )
    ON CONFLICT (tenant_id, job_id) DO UPDATE
    SET stack_id = EXCLUDED.stack_id,
        refreshed_at = EXCLUDED.refreshed_at;

    FOREACH source_table IN ARRAY ARRAY[
        'managed_runtime_capacity_reservations',
        'managed_runtime_capacity_release_facts',
        'techstack_vm_leases',
        'runtime_lease_execution_authorities',
        'managed_runtime_support_replacements',
        'servers',
        'jobs'
    ] LOOP
        EXECUTE pg_catalog.format(
            'DROP POLICY never_enrolled_runtime_seed ON %I.%I',
            active_schema, source_table
        );
    END LOOP;
END
$never_enrolled_runtime_seed$;

DO $never_enrolled_runtime_posture$
DECLARE
    active_schema text := pg_catalog.current_schema();
    signature text;
    runtime_role text;
BEGIN
    FOREACH signature IN ARRAY ARRAY[
        'provider_control_rebuild_never_enrolled_runtime_lease(text,text)',
        'provider_control_refresh_never_enrolled_runtime_leases()',
        'provider_control_refresh_never_enrolled_runtime_server()',
        'provider_control_refresh_never_enrolled_runtime_job()',
        'provider_control_list_never_enrolled_runtime_candidates(text,text,integer,integer)'
    ] LOOP
        EXECUTE pg_catalog.format('ALTER FUNCTION %I.%s OWNER TO %I', active_schema, signature, current_user);
        EXECUTE pg_catalog.format('ALTER FUNCTION %I.%s SET search_path TO pg_catalog, %I, pg_temp', active_schema, signature, active_schema);
        EXECUTE pg_catalog.format('REVOKE ALL ON FUNCTION %I.%s FROM PUBLIC', active_schema, signature);
    END LOOP;

    -- The read keeps the EXECUTE grant of 205 and the provider bootstrap; a
    -- forward homelab_ runtime role receives the same single capability.
    FOREACH runtime_role IN ARRAY ARRAY[
        'techstack_provider_control_runtime',
        'homelab_provider_control_runtime'
    ] LOOP
        IF pg_catalog.to_regrole(runtime_role) IS NULL THEN
            CONTINUE;
        END IF;
        EXECUTE pg_catalog.format(
            'GRANT EXECUTE ON FUNCTION %I.provider_control_list_never_enrolled_runtime_candidates(text, text, integer, integer) TO %I',
            active_schema, runtime_role
        );
    END LOOP;
END
$never_enrolled_runtime_posture$;

COMMENT ON TABLE provider_never_enrolled_runtime_leases IS
    'Secret-free migration-owned directory of unreleased provider-control leases that may still be never-enrolled candidates (B1).';
COMMENT ON TABLE provider_never_enrolled_runtime_server_evidence IS
    'Secret-free migration-owned directory of servers whose own row proves enrollment or decommission of their lease (B1).';
COMMENT ON TABLE provider_never_enrolled_runtime_active_jobs IS
    'Secret-free migration-owned directory of pending, running or waiting jobs per stack (B1).';
COMMENT ON FUNCTION provider_control_rebuild_never_enrolled_runtime_lease(text, text) IS
    'Rebuilds the never-enrolled directory entry of one lease from the rows visible to its owner-run caller.';
COMMENT ON FUNCTION provider_control_refresh_never_enrolled_runtime_leases() IS
    'Refreshes the never-enrolled directory entries of the leases one written row names, under its exact FORCE-RLS tenant scope.';
COMMENT ON FUNCTION provider_control_refresh_never_enrolled_runtime_server() IS
    'Refreshes the enrollment or decommission evidence of one written server row under its exact FORCE-RLS tenant scope.';
COMMENT ON FUNCTION provider_control_refresh_never_enrolled_runtime_job() IS
    'Refreshes the active-job entry of one written job row under its exact FORCE-RLS tenant scope.';
COMMENT ON FUNCTION provider_control_list_never_enrolled_runtime_candidates(text, text, integer, integer) IS
    'Hard-limited read-only directory of unreleased provider-control capacity past its enrollment window whose canonical server never enrolled and which no non-terminal stack job is working on.';
