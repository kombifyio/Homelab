-- Managed runtime lease renewal directory (platform-2xnsxf).
--
-- Native admission gives every managed lease a database-time validity window
-- (30 days by default). Provider control refuses every action on a lease whose
-- window has passed, so a running, paid server lost provider control 30 days
-- after it was provisioned: nothing ever extended the window.
--
-- The provider-control runtime role and the migration owner are NOBYPASSRLS
-- and the lease table is FORCE ROW LEVEL SECURITY, so a SECURITY DEFINER read
-- of techstack_vm_leases sees no tenant's rows (093). Like 093, this keeps a
-- secret-free, migration-owned directory that the lease writes maintain while
-- they still carry their tenant scope, and exposes only a bounded keyset read
-- of it to provider control.
--
-- The directory holds one row per live subscription lease, from the lease row
-- alone: uncancelled, running or stopped, bound to an owner, a server and a
-- resource generation, with a validity window, no customer-substrate custody
-- and no decommission claim. Execution authority, released capacity and server
-- teardown live in other tables; the renewal re-checks all of them inside the
-- tenant-scoped transaction that extends the window, so a directory row alone
-- never renews anything. The trigger touches only the written lease's own
-- directory row and takes no tenant-wide lock (163).

SET LOCAL lock_timeout = '5s';

CREATE TABLE provider_managed_lease_renewal_directory (
    tenant_id text NOT NULL CHECK (BTRIM(tenant_id) <> ''),
    lease_id text NOT NULL CHECK (BTRIM(lease_id) <> ''),
    owner_subject_id text NOT NULL CHECK (BTRIM(owner_subject_id) <> ''),
    provider_id text NOT NULL CHECK (BTRIM(provider_id) <> ''),
    resource_generation_id uuid NOT NULL,
    valid_until timestamptz NOT NULL,
    refreshed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, lease_id)
);

CREATE INDEX provider_managed_lease_renewal_directory_valid_until_idx
    ON provider_managed_lease_renewal_directory (valid_until);

REVOKE ALL ON TABLE provider_managed_lease_renewal_directory FROM PUBLIC;

CREATE FUNCTION provider_control_refresh_managed_lease_renewal_directory()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path FROM CURRENT
AS $$
DECLARE
    lease_tenant_id text := COALESCE(NEW.tenant_id, OLD.tenant_id);
    scoped_tenant_id text := NULLIF(current_setting('app.tenant_id', true), '');
BEGIN
    IF lease_tenant_id IS NULL OR BTRIM(lease_tenant_id) = '' THEN
        RAISE EXCEPTION 'managed lease renewal refresh requires an exact tenant'
            USING ERRCODE = '55000';
    END IF;
    IF scoped_tenant_id IS DISTINCT FROM lease_tenant_id THEN
        RAISE EXCEPTION 'managed lease renewal refresh requires the exact tenant scope'
            USING ERRCODE = '42501';
    END IF;

    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        DELETE FROM provider_managed_lease_renewal_directory AS entry
        WHERE entry.tenant_id = OLD.tenant_id
          AND entry.lease_id = OLD.id;
    END IF;

    IF TG_OP IN ('INSERT', 'UPDATE')
       AND NEW.cancelled_at IS NULL
       AND NEW.desired_state IN ('running', 'stopped')
       AND BTRIM(COALESCE(NEW.owner_subject_id, '')) <> ''
       AND BTRIM(COALESCE(NEW.server_id, '')) <> ''
       AND NEW.resource_generation_id IS NOT NULL
       AND NEW.valid_from IS NOT NULL
       AND NEW.valid_until IS NOT NULL
       AND NEW.lease_json->>'billing_mode' = 'subscription'
       AND NEW.lease_json->>'lifecycle_class' = 'subscription'
       AND COALESCE(NEW.lease_json->>'custody_class', '') = ''
       AND COALESCE(NEW.lease_json->'metadata'->>'resource_decommission_claim_digest', '') = '' THEN
        INSERT INTO provider_managed_lease_renewal_directory (
            tenant_id, lease_id, owner_subject_id, provider_id,
            resource_generation_id, valid_until, refreshed_at
        ) VALUES (
            NEW.tenant_id, NEW.id, NEW.owner_subject_id, NEW.provider_id,
            NEW.resource_generation_id, NEW.valid_until, clock_timestamp()
        );
    END IF;

    RETURN COALESCE(NEW, OLD);
END;
$$;

CREATE FUNCTION provider_control_list_managed_lease_renewal_candidates(
    after_tenant_id text,
    after_lease_id text,
    renewal_horizon_seconds integer,
    requested_limit integer
)
RETURNS TABLE (
    tenant_id text,
    owner_subject_id text,
    lease_id text,
    provider_id text,
    resource_generation_id uuid,
    valid_until timestamptz
)
LANGUAGE plpgsql
SECURITY DEFINER
STABLE
SET search_path FROM CURRENT
AS $$
BEGIN
    IF requested_limit IS NULL OR requested_limit < 1 OR requested_limit > 101 THEN
        RAISE EXCEPTION 'provider-control managed lease renewal limit must be between 1 and 101'
            USING ERRCODE = '22023';
    END IF;
    IF renewal_horizon_seconds IS NULL OR renewal_horizon_seconds < 3600 OR renewal_horizon_seconds > 1728000 THEN
        RAISE EXCEPTION 'provider-control managed lease renewal horizon must be between 3600 and 1728000 seconds'
            USING ERRCODE = '22023';
    END IF;

    RETURN QUERY
    SELECT
        entry.tenant_id,
        entry.owner_subject_id,
        entry.lease_id,
        entry.provider_id,
        entry.resource_generation_id,
        entry.valid_until
    FROM provider_managed_lease_renewal_directory AS entry
    WHERE entry.valid_until < clock_timestamp() + make_interval(secs => renewal_horizon_seconds)
      AND (entry.tenant_id, entry.lease_id) > (
          COALESCE(BTRIM(after_tenant_id), ''),
          COALESCE(BTRIM(after_lease_id), '')
      )
    ORDER BY entry.tenant_id, entry.lease_id
    LIMIT requested_limit;
END;
$$;

ALTER FUNCTION provider_control_refresh_managed_lease_renewal_directory()
    SET search_path TO pg_catalog, public, pg_temp;
ALTER FUNCTION provider_control_list_managed_lease_renewal_candidates(text, text, integer, integer)
    SET search_path TO pg_catalog, public, pg_temp;
REVOKE ALL ON FUNCTION provider_control_refresh_managed_lease_renewal_directory()
    FROM PUBLIC;
REVOKE ALL ON FUNCTION provider_control_list_managed_lease_renewal_candidates(text, text, integer, integer)
    FROM PUBLIC;

-- Seed the directory from the live leases. The lock blocks lease writes until
-- commit, so no write falls between the seed and the trigger. The owner-only
-- read policy exists only inside this transaction (196).
LOCK TABLE techstack_vm_leases IN SHARE ROW EXCLUSIVE MODE;

CREATE TRIGGER runtime_leases_refresh_managed_lease_renewal_directory
AFTER INSERT OR UPDATE OR DELETE ON techstack_vm_leases
FOR EACH ROW EXECUTE FUNCTION provider_control_refresh_managed_lease_renewal_directory();

DO $managed_lease_renewal_seed$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class AS lease_table
        WHERE lease_table.oid = pg_catalog.to_regclass('techstack_vm_leases')
          AND lease_table.relowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
          AND lease_table.relrowsecurity AND lease_table.relforcerowsecurity) THEN
        RAISE EXCEPTION 'managed lease renewal seed requires the migration owner and FORCE RLS'
            USING ERRCODE = '42501';
    END IF;
    EXECUTE pg_catalog.format(
        'CREATE POLICY managed_lease_renewal_seed ON techstack_vm_leases FOR SELECT TO %I USING (true)',
        current_user
    );
END
$managed_lease_renewal_seed$;

INSERT INTO provider_managed_lease_renewal_directory (
    tenant_id, lease_id, owner_subject_id, provider_id,
    resource_generation_id, valid_until, refreshed_at
)
SELECT
    lease.tenant_id, lease.id, lease.owner_subject_id, lease.provider_id,
    lease.resource_generation_id, lease.valid_until, clock_timestamp()
FROM techstack_vm_leases AS lease
WHERE lease.cancelled_at IS NULL
  AND lease.desired_state IN ('running', 'stopped')
  AND BTRIM(COALESCE(lease.owner_subject_id, '')) <> ''
  AND BTRIM(COALESCE(lease.server_id, '')) <> ''
  AND lease.resource_generation_id IS NOT NULL
  AND lease.valid_from IS NOT NULL
  AND lease.valid_until IS NOT NULL
  AND lease.lease_json->>'billing_mode' = 'subscription'
  AND lease.lease_json->>'lifecycle_class' = 'subscription'
  AND COALESCE(lease.lease_json->>'custody_class', '') = ''
  AND COALESCE(lease.lease_json->'metadata'->>'resource_decommission_claim_digest', '') = '';

DROP POLICY managed_lease_renewal_seed ON techstack_vm_leases;

COMMENT ON TABLE provider_managed_lease_renewal_directory IS
    'Secret-free migration-owned directory of live subscription leases and their validity windows (platform-2xnsxf).';
COMMENT ON FUNCTION provider_control_refresh_managed_lease_renewal_directory() IS
    'Refreshes the renewal directory row of one written lease under its exact FORCE-RLS tenant scope.';
COMMENT ON FUNCTION provider_control_list_managed_lease_renewal_candidates(text, text, integer, integer) IS
    'Hard-limited read-only keyset directory of live subscription leases whose validity window ends within the renewal horizon or has already ended.';
