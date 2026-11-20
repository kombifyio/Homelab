-- Revision marker and data cleanup for the consuming binary (no staff gating,
-- platform-0lkjel). guard_reach_states.owner_roles is no longer read or
-- written; contract migration 198 drops it once every live revision reports
-- this number. A kombify platform role is no membership role either: every
-- Cloud membership is a plain member and keeps no stored platform_roles. The
-- local owner membership of a self-hosted installation keeps its admin role.
-- The binary never lets a Cloud membership contribute a role, whatever is
-- stored (cmd/techstack appendMembershipRole), so this cleanup decides nothing:
-- when its login may not perform it, it reports a notice and skips, never a
-- failed release.
SET LOCAL lock_timeout = '5s';
DO $platform_roles$
DECLARE
    forced boolean;
    owned boolean;
    restricted boolean;
    changed bigint;
BEGIN
    SELECT c.relforcerowsecurity, c.relowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
      INTO forced, owned
      FROM pg_catalog.pg_class c
     WHERE c.oid = pg_catalog.to_regclass('public.techstack_memberships');
    IF forced IS NULL THEN
        RETURN;
    END IF;
    IF forced THEN
        SELECT EXISTS (
            SELECT 1 FROM pg_catalog.pg_policy p
             WHERE p.polrelid = pg_catalog.to_regclass('public.techstack_memberships')
               AND p.polcmd IN ('r', 'w', '*') AND NOT p.polpermissive
        ) INTO restricted;
        IF NOT owned OR restricted THEN
            RAISE NOTICE 'membership platform role cleanup skipped: owned=% restricted_policy=%', owned, restricted;
            RETURN;
        END IF;
    END IF;
    BEGIN
        IF forced THEN
            EXECUTE pg_catalog.format('CREATE POLICY platform_role_cleanup ON public.techstack_memberships FOR ALL TO %I USING (true) WITH CHECK (true)', current_user);
        END IF;
        UPDATE public.techstack_memberships
           SET role_key = 'member', metadata_json = metadata_json - 'platform_roles'
         WHERE provider_key = 'cloud' AND (role_key <> 'member' OR jsonb_exists(metadata_json, 'platform_roles'));
        GET DIAGNOSTICS changed = ROW_COUNT;
        IF forced THEN
            EXECUTE 'DROP POLICY platform_role_cleanup ON public.techstack_memberships';
        END IF;
        RAISE NOTICE 'membership platform role cleanup: % cloud membership row(s) set to member', changed;
    EXCEPTION WHEN insufficient_privilege THEN
        RAISE NOTICE 'membership platform role cleanup skipped: %', SQLERRM;
    END;
END $platform_roles$;
