-- Retain the secret-free tenant directory after outbox history is pruned.
-- The runtime keeps dormant rows at infinity while servers still exist; new
-- outbox events wake them through the existing LEAST trigger. Re-seed tenants
-- already removed by the old compactor, without exposing tenant payloads.
SET LOCAL lock_timeout = '5s';
SELECT pg_catalog.set_config('search_path', pg_catalog.quote_ident(pg_catalog.current_schema()) || ', pg_catalog, pg_temp', true);
LOCK TABLE servers IN SHARE ROW EXCLUSIVE MODE;
DO $directory_owner$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.oid = to_regclass('servers')
        AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
        AND c.relrowsecurity AND c.relforcerowsecurity) THEN
        RAISE EXCEPTION 'registry directory backfill requires migration owner and FORCE RLS' USING ERRCODE = '42501';
    END IF;
    EXECUTE format('CREATE POLICY registry_directory_backfill ON servers FOR SELECT TO %I USING (true)', current_user);
END $directory_owner$;
INSERT INTO server_registry_outbox_prune_tenants(tenant_id, oldest_created_at)
SELECT DISTINCT tenant_id, 'infinity'::timestamptz FROM servers
ON CONFLICT (tenant_id) DO NOTHING;
DROP POLICY registry_directory_backfill ON servers;
