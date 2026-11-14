-- 192_home_graph_state.additive.sql
--
-- The latest kombify home graph per owner and space (HOME-GRAPH-TARGET.md,
-- D-2026-11-09-03) and the state of its sync to the owner's Home Assistant.
-- Cloud writes the graph through the private household home-graph route; the
-- application_home_graph_sync job projects it through the node and stores the
-- node's answer.
--
--   graph, graph_revision  the latest accepted kombify.home_graph.v1 document
--                          and its revision; a lower revision never replaces it
--   restore                drift items the owner chose to resolve with the
--                          kombify value; kept until one sync ran with them
--   status                 pending (a revision or a restore waits), synced
--                          (the node answered for synced_revision), failed
--                          (the last run failed; error_code says why)
--   result                 the validated stackkit.home-graph-sync-result/v1
--                          of the last successful run (links, drift, inventory)
--
-- No credential is stored: the Home Assistant token stays in node custody.
--   failure_count          consecutive failed runs; drives the retry backoff
--                          (5 minutes, doubling, capped at 6 hours) and resets
--                          on a new revision, a new restore or a success
--
-- Row security: this table has only the permissive tenant policy
-- (app.tenant_id), not the verified-identity policy of migration 188. The
-- Cloud servicecall path carries no kombify_ctx token, so a verified-identity
-- policy could never admit it. Every access goes through the store's
-- withTenant, which sets app.tenant_id for the transaction.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '2min';
CREATE TABLE home_graph_state (
    tenant_id text NOT NULL REFERENCES techstack_tenants(id) ON DELETE CASCADE,
    owner_subject_id text NOT NULL,
    space_id text NOT NULL,
    graph jsonb NOT NULL,
    graph_revision bigint NOT NULL,
    restore jsonb NOT NULL DEFAULT '[]'::jsonb,
    status text NOT NULL DEFAULT 'pending',
    synced_revision bigint NOT NULL DEFAULT -1,
    result jsonb,
    error_code text,
    failure_count integer NOT NULL DEFAULT 0,
    synced_at timestamptz,
    last_attempt_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, owner_subject_id, space_id),
    CHECK (BTRIM(owner_subject_id) <> ''),
    CHECK (space_id ~ '^spc_[a-f0-9]{24}$'),
    CHECK (jsonb_typeof(graph) = 'object'),
    CHECK (graph_revision >= 0),
    CHECK (jsonb_typeof(restore) = 'array' AND jsonb_array_length(restore) <= 256),
    CHECK (status IN ('pending', 'synced', 'failed')),
    CHECK (synced_revision >= -1),
    CHECK (failure_count >= 0),
    CHECK (result IS NULL OR jsonb_typeof(result) = 'object'),
    CHECK (error_code IS NULL OR error_code ~ '^[a-z][a-z0-9_]{0,63}$')
);

CREATE INDEX home_graph_state_owner ON home_graph_state (tenant_id, owner_subject_id);

ALTER TABLE home_graph_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE home_graph_state FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON home_graph_state
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
