-- The hosted system login techstack_runtime_system carries Techstack's own
-- work that has no Edge-signed database context: background loops, Guard
-- ingress, Cloud service-to-service routes and the Guard gRPC listener. It is
-- registered here as a system role; it is never a context-token audience.
-- Login creation and password custody stay with the governed infrastructure
-- bootstrap, like techstack_application and techstack_backup_worker; database
-- delivery grants it the same finite application capability. No policy here
-- widens any role: the existing tenant policies scope it by its own
-- transaction tenant, and contract 191 then removes the hosted application
-- login's token-less arms on servers, stacks and services.
SET LOCAL lock_timeout = '5s';

INSERT INTO kombify_ctx.role_class(role_name, class)
VALUES ('techstack_runtime_system', 'system')
ON CONFLICT (role_name) DO NOTHING;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM kombify_ctx.role_class
        WHERE role_name = 'techstack_runtime_system' AND class = 'system'
    ) THEN
        RAISE EXCEPTION 'runtime system role class mismatch';
    END IF;
END $$;
