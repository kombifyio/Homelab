-- kombify:contract objects=public.guard_reach_states.owner_roles
-- The Connect staff cohort is gone: the control plane no longer captures the
-- owner's roles at pairing (no staff gating, platform-0lkjel). From 198 no
-- code reads or writes the column; the stored platform roles go with it.
SET LOCAL lock_timeout = '5s';

ALTER TABLE public.guard_reach_states DROP COLUMN IF EXISTS owner_roles;
