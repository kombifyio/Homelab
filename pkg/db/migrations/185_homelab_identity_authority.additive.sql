-- 185_homelab_identity_authority.additive.sql
--
-- The homelab authority pointer (ADR-036; workspace
-- HOMELAB-IDENTITY-ACCESS-TARGET.md sections 4 and 5; kombify-StackKits-nvih).
-- A homelab has exactly one Pocket ID, on its identity Authority. Techstack
-- records which server that is, the StackKit deployment that runs Pocket ID
-- there, and the homelab's identity origin (#IdentityOriginV1: the Pocket ID
-- host id.<name> and its source kombify-me, own-domain, local-only for
-- lab.home, or reserved-name).
--
--   identity_authority_server_id    the server that runs the homelab's Pocket ID
--   identity_authority_stack_id     the StackKit deployment that runs it there
--   identity_origin_name            the homelab name; the Pocket ID host is id.<name>
--   identity_origin_source          kombify-me, own-domain, local-only or reserved-name
--   identity_authority_observed_at  Guard observation time of the report that
--                                   last set the pointer or origin
--
-- The Guard inventory of the Authority server projects it inside the fenced
-- inventory satellite transaction (pkg/controlplane): only a host-key-bound
-- Guard whose StackKit access manifest declares the managed Pocket ID, only
-- for its own server and the homelab Techstack's own stack record names.
-- NULL means no Authority has reported yet. The vocabulary is enforced by the
-- writer and the reader: an additive migration adds no constraint to an
-- existing table.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '2min';
ALTER TABLE homelabs
    ADD COLUMN identity_authority_server_id text,
    ADD COLUMN identity_authority_stack_id text,
    ADD COLUMN identity_origin_name text,
    ADD COLUMN identity_origin_source text,
    ADD COLUMN identity_authority_observed_at timestamptz;
