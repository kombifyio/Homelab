-- 187_guard_reach_agent_link.additive.sql
--
-- The homelab identity agent link (workspace HOMELAB-IDENTITY-ACCESS-TARGET.md
-- section 7; Gateway ADR 0042 "Agent node envelope contract";
-- kombify-StackKits-igti.12). The Gateway answers the Connect registration of
-- a Guard-managed server with the node's agent envelope verification key.
-- Techstack keeps that key per server on its reach row and delivers it, with
-- the D10 references, to the node over the Guard (StackKit
-- AGENT_LINK_INSTALL); the node pins it.
--
--   agent_envelope_key     canonical JSON of the Gateway's agent_envelope key
--                          document of the current registration (public key,
--                          node id, kid, thumbprint, generation); '' while the
--                          Gateway hands none
--   agent_link_state       '' (never delivered), pending (new key or owner
--                          waiting for the Owner's passkey step-up on the
--                          node), registered (pinned), failed or revoked
--                          (removed from the node)
--   agent_link_generation  binding generation of the delivered link
--   agent_link_digest      sha256:<hex> of the exact request bytes the node
--                          pinned, last failed on or removed
--   agent_link_reason      stable reason code of a failed delivery
--   agent_link_updated_at  last state change
--   agent_link_retry_at    earliest retry of a failed or Owner-pending delivery
--   agent_link_pinned      sha256:<hex> of the request the node last pinned
--                          ('' after a removal); it decides whether a pin
--                          has to be removed
--
-- Nothing here is a credential. The vocabulary is enforced by the writer and
-- the reader: an additive migration adds no constraint to an existing table.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '2min';
ALTER TABLE guard_reach_states
    ADD COLUMN agent_envelope_key text NOT NULL DEFAULT '',
    ADD COLUMN agent_link_state text NOT NULL DEFAULT '',
    ADD COLUMN agent_link_generation bigint NOT NULL DEFAULT 0,
    ADD COLUMN agent_link_digest text NOT NULL DEFAULT '',
    ADD COLUMN agent_link_reason text NOT NULL DEFAULT '',
    ADD COLUMN agent_link_updated_at timestamptz,
    ADD COLUMN agent_link_retry_at timestamptz,
    ADD COLUMN agent_link_pinned text NOT NULL DEFAULT '';
