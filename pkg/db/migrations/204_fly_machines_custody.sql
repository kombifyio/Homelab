SET LOCAL lock_timeout = '5s';

CREATE TABLE fly_machine_generations (
    tenant_id text NOT NULL,
    generation_id text NOT NULL UNIQUE,
    subject_id text NOT NULL,
    app_id text NOT NULL,
    fly_app text NOT NULL,
    machine_id text NOT NULL DEFAULT '',
    custody_hash text NOT NULL,
    spec jsonb NOT NULL,
    released boolean NOT NULL DEFAULT false,
    absence_evidence_ref text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, generation_id),
    CHECK (btrim(tenant_id) <> ''),
    CHECK (btrim(generation_id) <> ''),
    CHECK (btrim(subject_id) <> ''),
    CHECK (btrim(app_id) <> ''),
    CHECK (fly_app ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CHECK (machine_id = '' OR machine_id ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CHECK (custody_hash ~ '^[a-f0-9]{64}$'),
    CHECK (jsonb_typeof(spec) = 'object'),
    CHECK (spec->>'region' = 'fra'),
    CHECK (spec->>'isolation_class' = 'firecracker'),
    CHECK ((spec->>'memory_mb')::integer IN (256, 512)),
    CHECK (NOT released OR btrim(absence_evidence_ref) <> '')
);

CREATE TABLE fly_machine_commands (
    tenant_id text NOT NULL,
    command_id text NOT NULL,
    fly_app text NOT NULL,
    generation_id text NOT NULL,
    subject_id text NOT NULL,
    app_id text NOT NULL,
    action text NOT NULL CHECK (action IN ('provision','start','stop','wake','observe','backup','restore','decommission')),
    request_digest text NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
    claim_until timestamptz NOT NULL,
    result jsonb,
    refusal jsonb,
    reconciled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, command_id)
);

CREATE INDEX fly_machine_generation_commands ON fly_machine_commands (tenant_id, generation_id, created_at);

CREATE FUNCTION fly_machine_generation_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.released THEN
            RAISE EXCEPTION 'new Fly Machine generation cannot be released';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.tenant_id, NEW.generation_id, NEW.subject_id, NEW.app_id, NEW.fly_app, NEW.custody_hash, NEW.spec, NEW.created_at)
        IS DISTINCT FROM ROW(OLD.tenant_id, OLD.generation_id, OLD.subject_id, OLD.app_id, OLD.fly_app, OLD.custody_hash, OLD.spec, OLD.created_at)
        OR (OLD.machine_id <> '' AND NEW.machine_id IS DISTINCT FROM OLD.machine_id)
        OR (OLD.released AND ROW(NEW.released, NEW.absence_evidence_ref) IS DISTINCT FROM ROW(OLD.released, OLD.absence_evidence_ref)) THEN
        RAISE EXCEPTION 'immutable Fly Machine generation';
    END IF;
    RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION fly_machine_generation_immutable() FROM PUBLIC;
CREATE TRIGGER fly_machine_generation_immutable BEFORE INSERT OR UPDATE ON fly_machine_generations
    FOR EACH ROW EXECUTE FUNCTION fly_machine_generation_immutable();

CREATE FUNCTION fly_machine_command_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    IF ROW(NEW.tenant_id, NEW.command_id, NEW.fly_app, NEW.generation_id, NEW.subject_id, NEW.app_id, NEW.action, NEW.request_digest, NEW.claim_until, NEW.created_at)
        IS DISTINCT FROM ROW(OLD.tenant_id, OLD.command_id, OLD.fly_app, OLD.generation_id, OLD.subject_id, OLD.app_id, OLD.action, OLD.request_digest, OLD.claim_until, OLD.created_at)
        OR (OLD.result IS NOT NULL AND NEW.result IS DISTINCT FROM OLD.result)
        OR (OLD.refusal IS NOT NULL AND NEW.refusal IS DISTINCT FROM OLD.refusal)
        OR (NEW.result IS NOT NULL AND NEW.refusal IS NOT NULL)
        OR (OLD.reconciled AND NOT NEW.reconciled) THEN
        RAISE EXCEPTION 'immutable Fly Machine command';
    END IF;
    RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION fly_machine_command_immutable() FROM PUBLIC;
CREATE TRIGGER fly_machine_command_immutable BEFORE UPDATE ON fly_machine_commands
    FOR EACH ROW EXECUTE FUNCTION fly_machine_command_immutable();

ALTER TABLE fly_machine_generations ENABLE ROW LEVEL SECURITY;
ALTER TABLE fly_machine_generations FORCE ROW LEVEL SECURITY;
ALTER TABLE fly_machine_commands ENABLE ROW LEVEL SECURITY;
ALTER TABLE fly_machine_commands FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON fly_machine_generations
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON fly_machine_commands
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY fly_machines_verified_identity ON fly_machine_generations AS RESTRICTIVE FOR ALL TO PUBLIC
    USING (session_user = 'techstack_application' AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()))
    WITH CHECK (session_user = 'techstack_application' AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()));
CREATE POLICY fly_machines_verified_identity ON fly_machine_commands AS RESTRICTIVE FOR ALL TO PUBLIC
    USING (session_user = 'techstack_application' AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()))
    WITH CHECK (session_user = 'techstack_application' AND tenant_id = (SELECT kombify_ctx.org_id()) AND subject_id = (SELECT kombify_ctx.subject()));

REVOKE ALL ON fly_machine_generations, fly_machine_commands FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'techstack_application') THEN
        GRANT SELECT, INSERT, UPDATE ON fly_machine_generations, fly_machine_commands TO techstack_application;
    END IF;
END $$;
