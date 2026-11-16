-- Admit an internal IONOS execution profile for eligible Standard requests.
-- The public offerings, existing execution profiles and their adapter pins
-- stay identical so previous binaries and in-flight operations keep working.
SET LOCAL lock_timeout = '5s';
SELECT pg_catalog.set_config(
    'search_path',
    pg_catalog.quote_ident(pg_catalog.current_schema()) || ', pg_catalog, pg_temp',
    true
);

INSERT INTO provider_catalog_versions (catalog_version, status)
VALUES ('managed-vps-2026-11-16.1', 'draft');

INSERT INTO provider_catalog_profiles (
    catalog_version, provider_id, adapter_id, credential_mode,
    runtime_profile_id, offering_id, can_pause, stop_effect, can_recreate,
    capability_snapshot, provision_dispatch_mode, adapter_manifest_hash
)
SELECT
    'managed-vps-2026-11-16.1', profile.provider_id, profile.adapter_id,
    profile.credential_mode, profile.runtime_profile_id, profile.offering_id,
    profile.can_pause, profile.stop_effect, profile.can_recreate,
    profile.capability_snapshot, profile.provision_dispatch_mode, profile.adapter_manifest_hash
FROM provider_catalog_profiles AS profile
JOIN provider_catalog_versions AS version USING (catalog_version)
WHERE version.status = 'active';

INSERT INTO provider_catalog_profiles (
    catalog_version, provider_id, adapter_id, credential_mode,
    runtime_profile_id, offering_id, can_pause, stop_effect, can_recreate,
    capability_snapshot, provision_dispatch_mode, adapter_manifest_hash
)
SELECT
    catalog_version, provider_id, 'ionos-cloudapi-v6-memory-s', credential_mode,
    'ionos-memory-cube-s-standard', 'ionos-cube-standard-memory',
    can_pause, stop_effect, can_recreate,
    jsonb_set(capability_snapshot, '{instance_types}', '["memory-cube-s"]'::jsonb),
    provision_dispatch_mode, 'sha256:6167cd0c480813ea24c3730bde830958fe0ff2042f572a4e89ba53606d4e5fed'
FROM provider_catalog_profiles
WHERE catalog_version = 'managed-vps-2026-11-16.1'
  AND provider_id = 'ionos'
  AND adapter_id = 'ionos-cloudapi-v6'
  AND credential_mode = 'managed'
  AND runtime_profile_id = 'ionos-basic-cube-standard'
  AND offering_id = 'monthly-runtime-standard'
  AND provision_dispatch_mode = 'at_most_once_dispatch_manual_reconcile'
  AND adapter_manifest_hash = 'sha256:640fcf4852e807ed28fa571bdad0ff2b7a21d74a4be975f2729ca5c6e3aef8ad'
  AND can_pause AND stop_effect = 'pause' AND can_recreate
  AND capability_snapshot -> 'instance_types' = '["basic-cube-s"]'::jsonb
  AND capability_snapshot -> 'regions' = '["de-fra-2"]'::jsonb;

DO $$
BEGIN
    IF (SELECT count(*) FROM provider_catalog_profiles
        WHERE catalog_version = 'managed-vps-2026-11-16.1'
          AND offering_id = 'ionos-cube-standard-memory') <> 1
       OR NOT EXISTS (
        SELECT 1 FROM provider_catalog_profiles
        WHERE catalog_version = 'managed-vps-2026-11-16.1'
          AND provider_id = 'ionos'
          AND runtime_profile_id = 'ionos-memory-cube-s-standard'
          AND offering_id = 'ionos-cube-standard-memory'
          AND adapter_id = 'ionos-cloudapi-v6-memory-s'
    ) THEN
        RAISE EXCEPTION 'Memory Cube S requires one exact IONOS Standard execution profile';
    END IF;
END;
$$;

UPDATE provider_catalog_versions
SET status = 'retired', retired_at = clock_timestamp()
WHERE status = 'active';

UPDATE provider_catalog_versions
SET status = 'active', activated_at = clock_timestamp()
WHERE catalog_version = 'managed-vps-2026-11-16.1' AND status = 'draft';

-- Earlier binaries only resolve the public offering. Fence their writes to a
-- Memory lease, including new power and decommission operations. The existing
-- dispatch guard still verifies the exact manifest of the selected row.
CREATE FUNCTION provider_operation_memory_cube_profile_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path FROM CURRENT
AS $$
DECLARE
    lease_provider text;
    public_offering text;
    execution_offering text;
    provider_package text;
BEGIN
    SELECT lease.provider_id,
           lease.lease_json #>> '{metadata,runtime_offering_id}',
           lease.lease_json #>> '{metadata,provider_execution_offering_id}',
           lease.lease_json #>> '{metadata,provider_package}'
    INTO STRICT lease_provider, public_offering, execution_offering, provider_package
    FROM techstack_vm_leases AS lease
    WHERE lease.tenant_id = NEW.tenant_id AND lease.id = NEW.lease_id;

    IF NULLIF(execution_offering, '') IS NULL
       AND provider_package IS DISTINCT FROM 'Memory Cube S'
       AND NEW.adapter_id IS DISTINCT FROM 'ionos-cloudapi-v6-memory-s'
       AND NEW.command_json #>> '{execution_profile,adapter_id}' IS DISTINCT FROM 'ionos-cloudapi-v6-memory-s'
       AND NEW.command_json #>> '{execution_profile,offering_id}' IS DISTINCT FROM 'ionos-cube-standard-memory' THEN
        RETURN NEW;
    END IF;

    IF lease_provider IS DISTINCT FROM 'ionos'
       OR public_offering IS DISTINCT FROM 'monthly-runtime-standard'
       OR execution_offering IS DISTINCT FROM 'ionos-cube-standard-memory'
       OR provider_package IS DISTINCT FROM 'Memory Cube S'
       OR NEW.adapter_id IS DISTINCT FROM 'ionos-cloudapi-v6-memory-s'
       OR NEW.command_json #>> '{command,provider_id}' IS DISTINCT FROM 'ionos'
       OR NEW.command_json #>> '{command,adapter_id}' IS DISTINCT FROM 'ionos-cloudapi-v6-memory-s'
       OR NEW.command_json #>> '{execution_profile,provider_id}' IS DISTINCT FROM 'ionos'
       OR NEW.command_json #>> '{execution_profile,adapter_id}' IS DISTINCT FROM 'ionos-cloudapi-v6-memory-s'
       OR NEW.command_json #>> '{execution_profile,runtime_profile_id}' IS DISTINCT FROM 'ionos-memory-cube-s-standard'
       OR NEW.command_json #>> '{execution_profile,offering_id}' IS DISTINCT FROM 'ionos-cube-standard-memory' THEN
        RAISE EXCEPTION 'provider operation requires its exact IONOS Memory Cube S execution profile'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER provider_operations_memory_cube_profile_guard
BEFORE INSERT ON provider_operations
FOR EACH ROW EXECUTE FUNCTION provider_operation_memory_cube_profile_guard();

-- Native admission has already inserted the new provision operation by the
-- time it binds the slot generation, in the same transaction. Use the prior
-- slot's immutable provision pin to catch old recreate writers that omit the
-- Memory selection when they construct the new lease.
CREATE FUNCTION managed_runtime_slot_memory_cube_continuity_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path FROM CURRENT
AS $$
BEGIN
    IF NEW.generation_ordinal > 1
       AND EXISTS (
           SELECT 1
           FROM managed_runtime_server_slot_generations AS prior
           JOIN provider_operations AS provision
             ON provision.tenant_id = prior.tenant_id
            AND provision.operation_id = prior.provision_operation_id
            AND provision.lease_id = prior.lease_id
           WHERE prior.tenant_id = NEW.tenant_id
             AND prior.slot_id = NEW.slot_id
             AND prior.generation_ordinal = NEW.generation_ordinal - 1
             AND provision.operation = 'provision'
             AND provision.adapter_id = 'ionos-cloudapi-v6-memory-s'
       )
       AND NOT EXISTS (
           SELECT 1 FROM provider_operations AS provision
           WHERE provision.tenant_id = NEW.tenant_id
             AND provision.operation_id = NEW.provision_operation_id
             AND provision.lease_id = NEW.lease_id
             AND provision.operation = 'provision'
             AND provision.adapter_id = 'ionos-cloudapi-v6-memory-s'
       ) THEN
        RAISE EXCEPTION 'managed runtime recreate requires its prior IONOS Memory Cube S execution profile'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER managed_runtime_slot_memory_cube_continuity_guard
BEFORE INSERT ON managed_runtime_server_slot_generations
FOR EACH ROW EXECUTE FUNCTION managed_runtime_slot_memory_cube_continuity_guard();
