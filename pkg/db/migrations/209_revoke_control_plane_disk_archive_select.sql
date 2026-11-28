-- Contract the expand-only archive readback introduced by migration 207.
-- The runtime now verifies archive rows only through the bounded digest
-- routine; application roles retain INSERT without access to content bytes.
--
-- Placed in the forward sequence (not migrations/contract/) because
-- InstallApplicationCapabilities and native PrepareApplication verify exact
-- privilege equality against application_privileges.go immediately after
-- Migrate. Once applied, rolling back to homelab <= 0.52.10 (whose privilege
-- verifier still requires SELECT,INSERT on control_plane_disk_archive) is not
-- supported.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '2min';
SELECT pg_catalog.set_config(
    'search_path',
    pg_catalog.quote_ident(pg_catalog.current_schema()) || ', pg_catalog, pg_temp',
    true
);

DO $control_plane_disk_archive_contract$
DECLARE
    active_schema text := pg_catalog.current_schema();
    application_role text;
    archive_table text := pg_catalog.format('%I.control_plane_disk_archive', active_schema);
    digest_function text := pg_catalog.format('%I.techstack_control_plane_disk_archive_digest(text,text)', active_schema);
    had_insert boolean;
BEGIN
    FOREACH application_role IN ARRAY ARRAY[
        'techstack_application',
        'homelab_application',
        'homelab_local_application'
    ] LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = application_role
        ) THEN
            CONTINUE;
        END IF;
        IF EXISTS (
            SELECT 1 FROM pg_catalog.pg_roles
            WHERE rolname = application_role AND (rolsuper OR rolbypassrls)
        ) THEN
            RAISE EXCEPTION 'disk archive contract refuses a superuser or BYPASSRLS application role: %', application_role
                USING ERRCODE = '42501';
        END IF;

        had_insert := pg_catalog.has_table_privilege(application_role, archive_table, 'INSERT');

        EXECUTE pg_catalog.format(
            'REVOKE SELECT ON TABLE %I.control_plane_disk_archive FROM %I',
            active_schema, application_role
        );

        -- Check effective privileges, including inherited and PUBLIC grants.
        -- Always fail if SELECT remains; assert INSERT and digest EXECUTE only
        -- for roles that already held archive INSERT before this migration
        -- (an unprepared role receives grants later in InstallApplicationCapabilities).
        IF pg_catalog.has_table_privilege(application_role, archive_table, 'SELECT')
           OR pg_catalog.has_any_column_privilege(application_role, archive_table, 'SELECT') THEN
            RAISE EXCEPTION 'disk archive contract requires SELECT revoked for %', application_role
                USING ERRCODE = '42501';
        END IF;
        IF had_insert THEN
            IF NOT pg_catalog.has_table_privilege(application_role, archive_table, 'INSERT') THEN
                RAISE EXCEPTION 'disk archive contract requires INSERT retained for %', application_role
                    USING ERRCODE = '42501';
            END IF;
            IF NOT pg_catalog.has_function_privilege(application_role, digest_function, 'EXECUTE') THEN
                RAISE EXCEPTION 'disk archive contract requires digest EXECUTE retained for %', application_role
                    USING ERRCODE = '42501';
            END IF;
        END IF;
    END LOOP;
END
$control_plane_disk_archive_contract$;
