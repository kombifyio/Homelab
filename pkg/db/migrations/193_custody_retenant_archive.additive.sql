-- 193_custody_retenant_archive.additive.sql
--
-- Verbatim archive of the provider custody of one managed runtime lease that
-- moves from a person's legacy tenant to the person's only tenant usr:<sub>
-- (platform-ilxzn, `techstack managed-runtime-retenant`).
--
-- Provider custody is hash-bound to its tenant (operation ids, command and
-- receipt digests, adapter evidence, slot ids, capacity and generation
-- digests), so it is never rewritten. The operator command copies every row of
-- the legacy custody graph here unchanged, removes it from the live tables in
-- the same transaction, and later re-admits the same lease and resource
-- generation under usr:<sub>; the provider resources are then adopted again by
-- the existing provision resolver with fresh evidence.
--
--   retenant_id      deterministic id of one move (from, to, lease, generation)
--   source_table     the live table the row came from
--   ordinal          position of the row in the archive manifest
--   row_json         to_jsonb() of the live row, unchanged
--   row_digest       sha256 of row_json::text
--
-- Only the migration owner reads or writes this schema: no application grant,
-- no tenant policy. The command refuses to run unless this table exists.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '2min';

CREATE SCHEMA IF NOT EXISTS techstack_archive;

CREATE TABLE techstack_archive.custody_retenant_rows (
    retenant_id text NOT NULL,
    from_tenant_id text NOT NULL,
    to_tenant_id text NOT NULL,
    subject_id text NOT NULL,
    lease_id text NOT NULL,
    resource_generation_id uuid NOT NULL,
    source_table text NOT NULL,
    ordinal integer NOT NULL,
    row_json jsonb NOT NULL,
    row_digest text NOT NULL,
    archived_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (retenant_id, ordinal)
);

CREATE INDEX custody_retenant_rows_lease_idx
    ON techstack_archive.custody_retenant_rows (lease_id, resource_generation_id);

COMMENT ON TABLE techstack_archive.custody_retenant_rows IS
    'Verbatim legacy provider custody of a managed runtime lease moved to usr:<sub> (platform-ilxzn). Append-only by convention; written only by techstack managed-runtime-retenant archive.';
