-- The disk-custody import maps a stack directory on the data disk to the
-- tenant that owns it. It can only ask the database which tenant owns a
-- stack under the tenant's own scope, so it must first learn which tenants
-- exist. The directory it used for that (drift_schedule_tenants) is filled by
-- a trigger on stacks rows, and it was never backfilled: a stack that
-- existed before that trigger holds no directory row, so its tenant is
-- invisible to the import and the stack directory stays unresolved. The
-- import then reports the stack as unowned, keeps `complete` false and the
-- disk stays mounted.
--
-- stacks is FORCE ROW LEVEL SECURITY. A SECURITY DEFINER function owned by
-- a role that does not hold BYPASSRLS reads no tenant's rows there: the
-- policy still applies to the definer (migration 201 records the same fact
-- for the lease table). So the directory of the stacks table cannot be a
-- function that reads stacks. It is a migration-owned, tenant-free
-- directory that the stack writes maintain while they still carry their
-- tenant scope, exactly like the drift and lease directories.
--
-- The directory holds one row per tenant that owns a stack. It holds no copy
-- of tenant data: a tenant identifier alone. It is derived from the stacks
-- rows, using INSERT/UPDATE superset semantics (no DELETE trigger, avoiding
-- lockless delete races and unscoped delete failures).
--
-- The seed reads the stacks rows inside this migration transaction, where the
-- migration owner holds the complete read (migration 201 seeds its directory
-- the same way). After the seed the trigger keeps it complete for every
-- later write.
--
-- control_plane_disk_archive holds tenant-free control-plane and remainder
-- files (including pb_data and unowned tofu state). This expand migration
-- adds a bounded SECURITY DEFINER digest-only readback routine that returns
-- metadata and sha256 only, never content.
--
-- Strictly expand-only: direct SELECT on control_plane_disk_archive is NOT
-- revoked here so the currently running production binary does not fail
-- VerifyRuntimeSystemLogin or archive read-back when the database leg applies
-- 207 before the new binary rolls out. REVOKE SELECT ON control_plane_disk_archive
-- belongs in the follow-up contract migration once all running binaries use
-- techstack_control_plane_disk_archive_digest.
--
-- Additive: a binary that predates this migration never calls the new
-- routines and keeps importing the way it did.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '2min';
SELECT pg_catalog.set_config(
    'search_path',
    pg_catalog.quote_ident(pg_catalog.current_schema()) || ', pg_catalog, pg_temp',
    true
);

CREATE TABLE IF NOT EXISTS stack_tenant_directory (
    tenant_id text PRIMARY KEY CHECK (BTRIM(tenant_id) <> ''),
    refreshed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

REVOKE ALL ON TABLE stack_tenant_directory FROM PUBLIC;

-- The stack write carries the exact tenant scope, so the refresh needs no
-- other tenant's row and no tenant-wide lock. Superset semantics (INSERT/UPDATE
-- only) match migration 153 and avoid lockless DELETE races.
CREATE OR REPLACE FUNCTION stack_tenant_directory_refresh()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path FROM CURRENT
AS $$
DECLARE
    stack_tenant_id text := NEW.tenant_id;
    scoped_tenant_id text := NULLIF(current_setting('app.tenant_id', true), '');
BEGIN
    IF TG_OP NOT IN ('INSERT', 'UPDATE')
       OR stack_tenant_id IS NULL
       OR BTRIM(stack_tenant_id) = '' THEN
        RAISE EXCEPTION 'stack tenant directory refresh requires an exact tenant'
            USING ERRCODE = '55000';
    END IF;
    IF scoped_tenant_id IS DISTINCT FROM stack_tenant_id
       OR (TG_OP = 'UPDATE' AND OLD.tenant_id IS DISTINCT FROM scoped_tenant_id) THEN
        RAISE EXCEPTION 'stack tenant directory refresh requires the exact tenant scope'
            USING ERRCODE = '42501';
    END IF;

    IF NEW.deleted_at IS NULL THEN
        INSERT INTO stack_tenant_directory (tenant_id, refreshed_at)
        VALUES (stack_tenant_id, clock_timestamp())
        ON CONFLICT (tenant_id) DO UPDATE SET refreshed_at = EXCLUDED.refreshed_at;
    END IF;

    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION stack_tenant_directory_refresh() FROM PUBLIC;

DROP TRIGGER IF EXISTS stack_tenant_directory_wake ON stacks;
CREATE TRIGGER stack_tenant_directory_wake
AFTER INSERT OR UPDATE ON stacks
FOR EACH ROW EXECUTE FUNCTION stack_tenant_directory_refresh();

-- The bounded keyset read of the directory. The runtime holds EXECUTE on
-- this function and no privilege on the table: it learns which tenants own
-- a stack and nothing else.
CREATE OR REPLACE FUNCTION techstack_list_stack_tenant_ids(
    after_tenant text,
    page_limit integer
)
RETURNS TABLE(tenant_id text)
LANGUAGE plpgsql
SECURITY DEFINER
STABLE
SET search_path FROM CURRENT
AS $$
BEGIN
    IF page_limit IS NULL OR page_limit < 1 OR page_limit > 100 THEN
        RAISE EXCEPTION 'bounded stack tenant page required' USING ERRCODE = '22023';
    END IF;
    RETURN QUERY
    SELECT entry.tenant_id
    FROM stack_tenant_directory AS entry
    WHERE entry.tenant_id > COALESCE(after_tenant, '')
    ORDER BY entry.tenant_id
    LIMIT page_limit;
END;
$$;
REVOKE ALL ON FUNCTION techstack_list_stack_tenant_ids(text, integer) FROM PUBLIC;

-- Bounded digest-only readback for control_plane_disk_archive. Returns version
-- count and verified digest/metadata for one exact (source_path, sha256) key;
-- never returns content bytes.
CREATE OR REPLACE FUNCTION techstack_control_plane_disk_archive_digest(
    p_source_path text,
    p_sha256 text
)
RETURNS TABLE(
    other_versions integer,
    content_sha256 text,
    byte_size bigint,
    archived_at timestamptz
)
LANGUAGE plpgsql
SECURITY DEFINER
STABLE
SET search_path FROM CURRENT
AS $$
BEGIN
    IF p_source_path IS NULL
       OR BTRIM(p_source_path) = ''
       OR left(p_source_path, 1) = '/'
       OR p_source_path ~ '(^|/)\.\.(/|$)' THEN
        RAISE EXCEPTION 'control-plane disk archive digest requires a valid relative path'
            USING ERRCODE = '22023';
    END IF;
    IF p_sha256 IS NULL OR p_sha256 !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'control-plane disk archive digest requires a sha256 hex digest'
            USING ERRCODE = '22023';
    END IF;

    RETURN QUERY
    SELECT
        (
            SELECT COUNT(*)::integer
            FROM control_plane_disk_archive AS other_row
            WHERE other_row.source_path = p_source_path
              AND other_row.sha256 <> p_sha256
        ),
        existing.row_sha256,
        existing.row_byte_size,
        existing.row_archived_at
    FROM (
        SELECT
            encode(pg_catalog.sha256(archive_row.content), 'hex') AS row_sha256,
            archive_row.byte_size AS row_byte_size,
            archive_row.archived_at AS row_archived_at
        FROM control_plane_disk_archive AS archive_row
        WHERE archive_row.source_path = p_source_path
          AND archive_row.sha256 = p_sha256
        LIMIT 1
    ) AS existing
    RIGHT JOIN (SELECT 1) AS one_row ON TRUE;
END;
$$;
REVOKE ALL ON FUNCTION techstack_control_plane_disk_archive_digest(text, text) FROM PUBLIC;

-- Seed the directory from the live stacks rows. The lock blocks stack writes
-- until commit, so no write falls between the seed and the trigger. The
-- owner-only read policy exists only inside this migration transaction.
LOCK TABLE stacks IN SHARE ROW EXCLUSIVE MODE;

DO $stack_tenant_directory_seed$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_class AS stack_table
        WHERE stack_table.oid = pg_catalog.to_regclass('stacks')
          AND stack_table.relowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
          AND stack_table.relrowsecurity AND stack_table.relforcerowsecurity
    ) THEN
        RAISE EXCEPTION 'stack tenant directory seed requires the migration owner and FORCE RLS'
            USING ERRCODE = '42501';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname = current_user AND (rolsuper OR rolbypassrls)
    ) THEN
        RAISE EXCEPTION 'stack tenant directory seed refuses a superuser or BYPASSRLS role'
            USING ERRCODE = '42501';
    END IF;
    EXECUTE pg_catalog.format(
        'CREATE POLICY stack_tenant_directory_seed ON stacks FOR SELECT TO %I USING (true)',
        current_user
    );
    INSERT INTO stack_tenant_directory (tenant_id, refreshed_at)
    SELECT grouped.tenant_id, clock_timestamp()
    FROM (
        SELECT stack.tenant_id
        FROM stacks AS stack
        WHERE BTRIM(COALESCE(stack.tenant_id, '')) <> ''
        GROUP BY stack.tenant_id
    ) AS grouped
    ON CONFLICT (tenant_id) DO UPDATE SET refreshed_at = EXCLUDED.refreshed_at;
    EXECUTE 'DROP POLICY stack_tenant_directory_seed ON stacks';
END
$stack_tenant_directory_seed$;

DO $stack_tenant_directory_posture$
DECLARE
    active_schema text := pg_catalog.current_schema();
    application_role text;
    signature text;
BEGIN
    FOREACH signature IN ARRAY ARRAY[
        'stack_tenant_directory_refresh()',
        'techstack_list_stack_tenant_ids(text,integer)',
        'techstack_control_plane_disk_archive_digest(text,text)'
    ] LOOP
        EXECUTE pg_catalog.format('ALTER FUNCTION %I.%s OWNER TO %I', active_schema, signature, current_user);
        EXECUTE pg_catalog.format('ALTER FUNCTION %I.%s SET search_path TO pg_catalog, %I, pg_temp', active_schema, signature, active_schema);
        EXECUTE pg_catalog.format('REVOKE ALL ON FUNCTION %I.%s FROM PUBLIC', active_schema, signature);
    END LOOP;

    FOREACH application_role IN ARRAY ARRAY[
        'techstack_application',
        'techstack_runtime_system',
        'homelab_application',
        'homelab_runtime_system',
        'homelab_local_application'
    ] LOOP
        IF pg_catalog.to_regrole(application_role) IS NULL THEN
            CONTINUE;
        END IF;
        IF EXISTS (
            SELECT 1 FROM pg_catalog.pg_roles
            WHERE rolname = application_role AND (rolsuper OR rolbypassrls)
        ) THEN
            RAISE EXCEPTION 'stack tenant directory refuses a superuser or BYPASSRLS application role'
                USING ERRCODE = '42501';
        END IF;
        -- Keep 207 strictly expand-only: REVOKE SELECT ON control_plane_disk_archive
        -- belongs in the follow-up contract migration once all running binaries
        -- use techstack_control_plane_disk_archive_digest.
        EXECUTE pg_catalog.format(
            'GRANT EXECUTE ON FUNCTION %I.techstack_list_stack_tenant_ids(text, integer) TO %I',
            active_schema, application_role
        );
        EXECUTE pg_catalog.format(
            'GRANT EXECUTE ON FUNCTION %I.techstack_control_plane_disk_archive_digest(text, text) TO %I',
            active_schema, application_role
        );
    END LOOP;
END
$stack_tenant_directory_posture$;

COMMENT ON TABLE stack_tenant_directory IS
    'Secret-free tenant directory of the stacks table: one tenant identifier per tenant that owns a stack (kombify-StackKits-cg3z).';
COMMENT ON FUNCTION techstack_list_stack_tenant_ids(text, integer) IS
    'Bounded keyset read of the tenant directory of the stacks table (kombify-StackKits-cg3z).';
COMMENT ON FUNCTION techstack_control_plane_disk_archive_digest(text, text) IS
    'Bounded digest-only verification of control_plane_disk_archive without exposing archived content bytes (kombify-StackKits-cg3z).';
