SET LOCAL lock_timeout = '5s';

CREATE TABLE ionos_pool_generations (
    tenant_id text NOT NULL,
    generation_id text NOT NULL UNIQUE,
    node_id text NOT NULL,
    spec jsonb NOT NULL,
    memory_mb integer NOT NULL CHECK (memory_mb BETWEEN 128 AND 512),
    released boolean NOT NULL DEFAULT false,
    absence_evidence_ref text NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id,generation_id),
    CHECK (spec->'workload'->>'tenant_id'=tenant_id),
    CHECK (spec->'workload'->>'handle'=generation_id),
    CHECK (spec->>'node_id'=node_id),
    CHECK ((spec->'workload'->>'memory_mb')::integer=memory_mb),
    CHECK (NOT released OR btrim(absence_evidence_ref)<>'')
);
CREATE TABLE ionos_pool_commands (
    tenant_id text NOT NULL,
    command_id text NOT NULL,
    node_id text NOT NULL,
    generation_id text NOT NULL,
    subject_id text NOT NULL,
    app_id text NOT NULL,
    action text NOT NULL CHECK (action IN ('provision','start','stop','observe','backup','restore','decommission')),
    request_digest text NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
    claim_until timestamptz NOT NULL,
    result jsonb,
    refusal jsonb,
    reconciled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id,command_id)
);
CREATE INDEX ionos_pool_generation_commands ON ionos_pool_commands(tenant_id,generation_id,created_at);

-- This non-tenant directory exposes only aggregate capacity, never identities,
-- images, manifests or generation handles. Only the custody trigger writes it.
CREATE TABLE ionos_pool_capacity (
    node_id text PRIMARY KEY,
    memory_mb integer NOT NULL CHECK(memory_mb>=0),
    slots integer NOT NULL CHECK(slots>=0)
);
CREATE FUNCTION ionos_pool_account_generation() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.tenant_id,NEW.generation_id,NEW.node_id,NEW.spec,NEW.memory_mb)
            IS DISTINCT FROM ROW(OLD.tenant_id,OLD.generation_id,OLD.node_id,OLD.spec,OLD.memory_mb)
            OR (OLD.released AND ROW(NEW.released,NEW.absence_evidence_ref) IS DISTINCT FROM ROW(OLD.released,OLD.absence_evidence_ref)) THEN
            RAISE EXCEPTION 'immutable IONOS generation';
        END IF;
        IF NOT OLD.released AND NEW.released THEN
            UPDATE public.ionos_pool_capacity SET memory_mb=memory_mb-NEW.memory_mb,slots=slots-1 WHERE node_id=NEW.node_id;
            IF NOT FOUND THEN RAISE EXCEPTION 'missing IONOS capacity custody'; END IF;
        END IF;
    ELSIF NOT NEW.released THEN
        INSERT INTO public.ionos_pool_capacity(node_id,memory_mb,slots) VALUES(NEW.node_id,NEW.memory_mb,1)
        ON CONFLICT(node_id) DO UPDATE SET memory_mb=ionos_pool_capacity.memory_mb+EXCLUDED.memory_mb,slots=ionos_pool_capacity.slots+1;
    ELSE
        RAISE EXCEPTION 'new IONOS generation cannot be released';
    END IF;
    RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION ionos_pool_account_generation() FROM PUBLIC;
CREATE TRIGGER ionos_pool_account_generation AFTER INSERT OR UPDATE ON ionos_pool_generations
    FOR EACH ROW EXECUTE FUNCTION ionos_pool_account_generation();

ALTER TABLE ionos_pool_generations ENABLE ROW LEVEL SECURITY;
ALTER TABLE ionos_pool_generations FORCE ROW LEVEL SECURITY;
ALTER TABLE ionos_pool_commands ENABLE ROW LEVEL SECURITY;
ALTER TABLE ionos_pool_commands FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ionos_pool_generations
    USING (tenant_id=current_setting('app.tenant_id',true)) WITH CHECK(tenant_id=current_setting('app.tenant_id',true));
CREATE POLICY tenant_isolation ON ionos_pool_commands
    USING (tenant_id=current_setting('app.tenant_id',true)) WITH CHECK(tenant_id=current_setting('app.tenant_id',true));
CREATE POLICY ionos_pool_verified_identity ON ionos_pool_generations AS RESTRICTIVE FOR ALL TO PUBLIC
    USING (session_user='techstack_application' AND tenant_id=(SELECT kombify_ctx.org_id()))
    WITH CHECK (session_user='techstack_application' AND tenant_id=(SELECT kombify_ctx.org_id()));
CREATE POLICY ionos_pool_verified_identity ON ionos_pool_commands AS RESTRICTIVE FOR ALL TO PUBLIC
    USING (session_user='techstack_application' AND tenant_id=(SELECT kombify_ctx.org_id()) AND subject_id=(SELECT kombify_ctx.subject()))
    WITH CHECK (session_user='techstack_application' AND tenant_id=(SELECT kombify_ctx.org_id()) AND subject_id=(SELECT kombify_ctx.subject()));
REVOKE ALL ON ionos_pool_generations,ionos_pool_commands,ionos_pool_capacity FROM PUBLIC;
DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='techstack_application') THEN
        GRANT SELECT,INSERT,UPDATE ON ionos_pool_generations,ionos_pool_commands TO techstack_application;
        GRANT SELECT ON ionos_pool_capacity TO techstack_application;
    END IF;
END $$;

CREATE FUNCTION ionos_pool_command_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
    IF ROW(NEW.tenant_id,NEW.command_id,NEW.node_id,NEW.generation_id,NEW.subject_id,NEW.app_id,NEW.action,NEW.request_digest,NEW.claim_until,NEW.created_at)
      IS DISTINCT FROM ROW(OLD.tenant_id,OLD.command_id,OLD.node_id,OLD.generation_id,OLD.subject_id,OLD.app_id,OLD.action,OLD.request_digest,OLD.claim_until,OLD.created_at)
      OR (OLD.result IS NOT NULL AND NEW.result IS DISTINCT FROM OLD.result)
      OR (OLD.refusal IS NOT NULL AND NEW.refusal IS DISTINCT FROM OLD.refusal)
      OR (NEW.result IS NOT NULL AND NEW.refusal IS NOT NULL)
      OR (OLD.reconciled AND NOT NEW.reconciled) THEN
        RAISE EXCEPTION 'immutable IONOS command';
    END IF;
    RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION ionos_pool_command_immutable() FROM PUBLIC;
CREATE TRIGGER ionos_pool_command_immutable BEFORE UPDATE ON ionos_pool_commands
    FOR EACH ROW EXECUTE FUNCTION ionos_pool_command_immutable();

-- One quota scope for every placement/tenant belonging to a Cloud account.
-- Future workload adapters must use this same reservation authority.
CREATE TABLE managed_app_slot_reservations (
 tenant_id text NOT NULL,
 generation_id text NOT NULL UNIQUE,
 account_id text NOT NULL,
 subject_id text NOT NULL,
 app_id text NOT NULL,
 placement text NOT NULL CHECK(placement IN ('ionos-shared-pool','fly','cloud-run')),
 allowance_limit integer NOT NULL CHECK(allowance_limit>0),
 allowance_until timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'held' CHECK(state IN ('held','released')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 released_at timestamptz,
 absence_evidence_ref text NOT NULL DEFAULT '',
 PRIMARY KEY(tenant_id,generation_id),
 CHECK((state='held' AND released_at IS NULL AND absence_evidence_ref='') OR (state='released' AND released_at IS NOT NULL AND btrim(absence_evidence_ref)<>''))
);
CREATE TABLE managed_app_slot_totals (
 account_id text PRIMARY KEY,
 held integer NOT NULL CHECK(held>=0)
);
ALTER TABLE managed_app_slot_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE managed_app_slot_reservations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON managed_app_slot_reservations
 USING(tenant_id=current_setting('app.tenant_id',true)) WITH CHECK(tenant_id=current_setting('app.tenant_id',true));
CREATE POLICY managed_app_slot_verified_identity ON managed_app_slot_reservations AS RESTRICTIVE FOR ALL TO PUBLIC
 USING(session_user='techstack_application' AND tenant_id=(SELECT kombify_ctx.org_id()) AND subject_id=(SELECT kombify_ctx.subject()))
 WITH CHECK(session_user='techstack_application' AND tenant_id=(SELECT kombify_ctx.org_id()) AND subject_id=(SELECT kombify_ctx.subject()));
CREATE FUNCTION managed_app_slot_account() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
 IF TG_OP='INSERT' THEN
   IF NEW.state<>'held' OR NEW.allowance_until<=clock_timestamp() OR NEW.allowance_until>clock_timestamp()+interval '30 seconds' THEN
     RAISE EXCEPTION 'fresh positive Managed App allowance required';
   END IF;
   INSERT INTO public.managed_app_slot_totals(account_id,held) VALUES(NEW.account_id,1)
   ON CONFLICT(account_id) DO UPDATE SET held=managed_app_slot_totals.held+1
     WHERE managed_app_slot_totals.held<NEW.allowance_limit;
   IF NOT FOUND THEN RAISE EXCEPTION 'Managed App allowance exhausted' USING ERRCODE='23514',CONSTRAINT='managed_app_slot_budget'; END IF;
 ELSE
   IF ROW(NEW.tenant_id,NEW.generation_id,NEW.account_id,NEW.subject_id,NEW.app_id,NEW.placement,NEW.allowance_limit,NEW.allowance_until,NEW.created_at)
     IS DISTINCT FROM ROW(OLD.tenant_id,OLD.generation_id,OLD.account_id,OLD.subject_id,OLD.app_id,OLD.placement,OLD.allowance_limit,OLD.allowance_until,OLD.created_at)
     OR (OLD.state='released' AND NEW IS DISTINCT FROM OLD) THEN
     RAISE EXCEPTION 'immutable Managed App reservation';
   END IF;
   IF OLD.state='held' AND NEW.state='released' THEN
     UPDATE public.managed_app_slot_totals SET held=held-1 WHERE account_id=NEW.account_id;
     IF NOT FOUND THEN RAISE EXCEPTION 'missing Managed App reservation count'; END IF;
   END IF;
 END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION managed_app_slot_account() FROM PUBLIC;
CREATE TRIGGER managed_app_slot_account AFTER INSERT OR UPDATE ON managed_app_slot_reservations
 FOR EACH ROW EXECUTE FUNCTION managed_app_slot_account();
REVOKE ALL ON managed_app_slot_reservations,managed_app_slot_totals FROM PUBLIC;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='techstack_application') THEN
   GRANT SELECT,INSERT,UPDATE ON managed_app_slot_reservations TO techstack_application;
 END IF;
END $$;
