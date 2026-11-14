-- 188_creation_drafts.additive.sql
--
-- The server-side creation draft (KOMBIFY-DESIGN-SYSTEM-STANDARD.md section 9;
-- kombify-StackKits-6u4m). A creation started in one frame (the Cloud popup,
-- a sheet, one step alone) continues in another one or on another device: the
-- draft keeps where the owner was and what they answered, per owner and
-- creation process.
--
--   draft_key        the creation process and its target as the wizard names
--                    it (stack-create, stack-install-<server>,
--                    stack-builder-<session>, add-use-case-<stack>,
--                    add-server-<stack>)
--   process          the process key the draft was last written from
--   step, furthest   the current step and the furthest step reached
--   answers_json     the non-secret answers; the writer refuses any field that
--                    carries a password, passphrase, token, key or secret
--   expires_at       after it the draft is gone for every reader; the next
--                    write of the owner removes expired drafts
--
-- A draft ends on an accepted create or when the owner discards it. Row
-- security is the one of the personal settings (migrations 128 and 167): the
-- tenant, and for the hosted application login the verified tenant and owner.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '2min';
CREATE TABLE creation_drafts (
    id text PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES techstack_tenants(id) ON DELETE CASCADE,
    owner_subject_id text NOT NULL,
    draft_key text NOT NULL,
    schema_version integer NOT NULL DEFAULT 1,
    process text NOT NULL,
    step text NOT NULL,
    furthest text NOT NULL,
    answers_json jsonb NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (BTRIM(id) <> ''),
    CHECK (BTRIM(owner_subject_id) <> ''),
    CHECK (draft_key ~ '^[a-z][A-Za-z0-9_.:-]{0,191}$'),
    CHECK (schema_version = 1),
    CHECK (process IN ('stack', 'place-on-server', 'add-server', 'add-server-found', 'join-on-server',
        'add-use-case', 'homelab-settings')),
    CHECK (step IN ('goals', 'server', 'access', 'users', 'login')),
    CHECK (furthest IN ('goals', 'server', 'access', 'users', 'login')),
    CHECK (jsonb_typeof(answers_json) = 'object'),
    CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX uq_creation_drafts_owner_key
    ON creation_drafts (tenant_id, owner_subject_id, draft_key);

ALTER TABLE creation_drafts ENABLE ROW LEVEL SECURITY;
ALTER TABLE creation_drafts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON creation_drafts
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY creation_drafts_verified_identity ON creation_drafts
    AS RESTRICTIVE FOR ALL TO PUBLIC
    USING (
        session_user <> 'techstack_application'
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND owner_subject_id = (SELECT kombify_ctx.subject())
        )
    )
    WITH CHECK (
        session_user <> 'techstack_application'
        OR (
            tenant_id = (SELECT kombify_ctx.org_id())
            AND owner_subject_id = (SELECT kombify_ctx.subject())
        )
    );
