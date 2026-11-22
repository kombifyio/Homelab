SET LOCAL lock_timeout = '5s';

ALTER TABLE services
    ADD COLUMN IF NOT EXISTS isolation_class text,
    ADD COLUMN IF NOT EXISTS ingress_hostname text,
    ADD COLUMN IF NOT EXISTS managed_service_slot_ref text;
