-- Proxmox substrate absence observations (kombify-StackKits-8t7b).
--
-- The Proxmox substrate adapter (internal/substrateruntime) proves a guest is
-- gone from a complete node-local VM list and records that read through
-- providerevidence.Recorder with provider_id 'proxmox'. Migration 041 admitted
-- only 'ionos' and 'centron', so every such insert violated
-- provider_absence_observations_provider_id_check (SQLSTATE 23514), the adapter
-- reported the decommission as pending, and a Proxmox decommission never left
-- absence_pending.
--
-- The constraint is widened to 'proxmox' and is otherwise unchanged. The
-- evidence and attestation references are provider-independent lookup refs of
-- the control plane, so no reference scheme changes. Widening only: every row
-- an older binary writes stays admitted. The table holds one row per
-- decommissioned binding, so the revalidation under the table lock is short.
--
-- Not widened: managed_runtime_capacity_reservations (033) and
-- managed_runtime_capacity_quarantine_retirements (055). Proxmox substrate
-- admission creates no commercial capacity reservation (migration 107) and 055
-- covers retired legacy simulate leases only. That the absent receipt
-- projection still requires such a reservation is a separate defect
-- (kombify-StackKits-mftm), not a constraint to widen here.

ALTER TABLE provider_absence_observations
    DROP CONSTRAINT provider_absence_observations_provider_id_check,
    ADD CONSTRAINT provider_absence_observations_provider_id_check
        CHECK (provider_id IN ('ionos', 'centron', 'proxmox'));
