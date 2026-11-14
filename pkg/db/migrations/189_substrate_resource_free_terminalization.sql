-- Proxmox substrate resource-free teardown (kombify-StackKits-chw7).
--
-- A customer-substrate provision that ends without any bound resource (still
-- requested or accepted, never dispatched) is closed by resource-free teardown.
-- Migration 035 made that teardown one atomic proof: the terminalization fact,
-- the RuntimeServer tombstone and a 'resource_free_teardown' managed-runtime
-- capacity release. Customer-owned hypervisor guests never hold a commercial
-- capacity reservation (migration 107), so neither the release fact nor the
-- terminalization row's foreign key to managed_runtime_capacity_reservations
-- could ever be satisfied, and such a lease never terminalized.
--
-- The foreign key to the reservation moves into the deferred commit fence,
-- which now admits exactly one of two release authorities:
--   * commercial capacity: the exact 'resource_free_teardown' release fact,
--     unchanged. The release fact keeps its own foreign key to the
--     reservation, so a commercial terminalization still proves one.
--   * customer substrate: exact substrate_guest_leases custody of this
--     provision (substrate_guest_custody_valid, migration 107) for the same
--     lease, RuntimeServer and resource generation, and no capacity
--     reservation for that generation. Nothing is released.
-- A lease that claims both, or neither, still fails closed. Widening only:
-- every row an older binary writes carries a reservation and its release fact
-- and stays admitted.

SET LOCAL lock_timeout = '5s';

DO $substrate_resource_free_reservation_fk$
DECLARE
    reservation_fk text;
    fk_count integer;
BEGIN
    SELECT pg_catalog.count(*), pg_catalog.min(constraint_row.conname)
    INTO fk_count, reservation_fk
    FROM pg_catalog.pg_constraint AS constraint_row
    WHERE constraint_row.conrelid =
            'provider_operation_resource_free_terminalizations'::regclass
      AND constraint_row.contype = 'f'
      AND constraint_row.confrelid =
            'managed_runtime_capacity_reservations'::regclass;
    IF fk_count IS DISTINCT FROM 1 THEN
        RAISE EXCEPTION 'resource-free terminalization reservation foreign key is not at the expected version (% found)', fk_count;
    END IF;
    EXECUTE pg_catalog.format(
        'ALTER TABLE provider_operation_resource_free_terminalizations DROP CONSTRAINT %I',
        reservation_fk
    );
END;
$substrate_resource_free_reservation_fk$;

CREATE OR REPLACE FUNCTION provider_resource_free_terminalization_validate_commit()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path FROM CURRENT
AS $$
BEGIN
    PERFORM 1
    FROM servers AS server
    WHERE server.tenant_id = NEW.tenant_id
      AND server.id = NEW.server_id
      AND server.lease_id = NEW.lease_id
      AND server.generation = NEW.server_generation
      AND server.desired_state = 'absent'
      AND server.lifecycle_state = 'decommissioned'
      AND server.decommissioned_at IS NOT NULL
      AND (
          EXISTS (
              SELECT 1
              FROM managed_runtime_capacity_release_facts AS release
              WHERE release.tenant_id = NEW.tenant_id
                AND release.lease_id = NEW.lease_id
                AND release.resource_generation_id = NEW.resource_generation_id
                AND release.server_id = NEW.server_id
                AND release.server_generation = NEW.server_generation
                AND release.release_operation_id = NEW.operation_id
                AND release.release_authority = 'resource_free_teardown'
                AND release.receipt_sequence = NEW.head_sequence
                AND release.receipt_digest = NEW.head_receipt_digest
          )
          OR (
              NOT EXISTS (
                  SELECT 1
                  FROM managed_runtime_capacity_reservations AS reservation
                  WHERE reservation.tenant_id = NEW.tenant_id
                    AND reservation.lease_id = NEW.lease_id
                    AND reservation.resource_generation_id = NEW.resource_generation_id
              )
              AND EXISTS (
                  SELECT 1
                  FROM substrate_guest_leases AS guest
                  WHERE guest.tenant_id = NEW.tenant_id
                    AND guest.lease_id = NEW.lease_id
                    AND guest.server_id = NEW.server_id
                    AND guest.resource_generation_id = NEW.resource_generation_id
                    AND guest.operation_id = NEW.operation_id
              )
              AND substrate_guest_custody_valid(
                  NEW.tenant_id,
                  NEW.lease_id,
                  NEW.resource_generation_id::text,
                  NEW.operation_id
              )
          )
      );
    IF NOT FOUND THEN
        RAISE EXCEPTION 'resource-free terminalization must atomically include its exact RuntimeServer tombstone and either its capacity release or exact customer substrate guest custody'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

-- Replacing the function keeps its owner and grants but takes the migration
-- transaction's search path. Restore the runtime boot posture of every
-- provider-control security boundary.
DO $substrate_resource_free_commit_posture$
DECLARE
    active_schema text := pg_catalog.current_schema();
BEGIN
    EXECUTE pg_catalog.format(
        'ALTER FUNCTION %I.provider_resource_free_terminalization_validate_commit() SECURITY DEFINER',
        active_schema
    );
    EXECUTE pg_catalog.format(
        'ALTER FUNCTION %I.provider_resource_free_terminalization_validate_commit() SET search_path TO pg_catalog, %I, pg_temp',
        active_schema,
        active_schema
    );
    EXECUTE pg_catalog.format(
        'REVOKE ALL ON FUNCTION %I.provider_resource_free_terminalization_validate_commit() FROM PUBLIC',
        active_schema
    );
END;
$substrate_resource_free_commit_posture$;

COMMENT ON FUNCTION provider_resource_free_terminalization_validate_commit() IS
    'Requires a resource-free terminalization to commit with its exact RuntimeServer tombstone and either its resource_free_teardown capacity release or exact customer substrate guest custody without a capacity reservation.';
