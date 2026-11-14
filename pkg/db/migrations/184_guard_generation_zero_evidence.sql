-- kombify Guard credential generation 0 (kombify-StackKits-vofh). A key-bound
-- Guard holds no opaque credential and authenticates at generation 0 until its
-- first rotation; StackKits #2776 made the control plane record that real
-- generation. Migrations 159 and 162 still admitted only generation >= 1, so
-- the round trip of every completed report of such a Guard violated
-- guard_command_round_trips_check (SQLSTATE 23514) and rolled the whole outcome
-- back: POST /api/v1/workers/{id}/commands/result answered 500 and no typed
-- StackKits command completed. Local repair evidence at generation 0 hit the
-- same refusal in guard_posture_repairs_local_binding_check.
--
-- Both constraints are widened to generation >= 0 and are otherwise unchanged.
-- Widening only: every row an older binary writes stays admitted, and an older
-- binary reads a generation-0 round trip as superseded, as it did before. The
-- round-trip table holds one row per Guard and the repair journal is small, so
-- the revalidation under the table lock is short.

ALTER TABLE guard_command_round_trips
    DROP CONSTRAINT guard_command_round_trips_check,
    ADD CONSTRAINT guard_command_round_trips_check CHECK (
        (transport = 'https' AND result_generation >= 0)
        OR (transport = 'mtls' AND result_generation = 0 AND BTRIM(credential_ref) <> '')
    );

ALTER TABLE guard_posture_repairs
    DROP CONSTRAINT guard_posture_repairs_local_binding_check,
    ADD CONSTRAINT guard_posture_repairs_local_binding_check CHECK (
        (mode = 'local'
            AND BTRIM(COALESCE(operation_id, '')) <> '' AND length(operation_id) <= 128
            AND BTRIM(COALESCE(policy_id, '')) <> '' AND policy_version > 0
            AND worker_generation >= 0 AND evidence_revision BETWEEN 1 AND 1000
            AND BTRIM(COALESCE(evidence_digest, '')) <> '')
        OR (mode <> 'local' AND operation_id IS NULL)
    );
