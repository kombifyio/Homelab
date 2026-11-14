-- 194_guard_power.additive.sql
--
-- kombify Guard UPS-aware power protection (workspace
-- GUARD-RELIABILITY-SECURITY-TARGET.md section 4.4 and the Guard I lane
-- kombify-StackKits-yihy.5). A Guard that reads a NUT UPS reports the power
-- state, the policy it applied and the measured ordered-shutdown steps on the
-- existing posture channel. The snapshot has its own epoch and sequence: a
-- power report never rewrites the security/backup cursor, and a replayed
-- snapshot is ignored.
--
--   power_state         the last accepted guardpower.PowerReport, or NULL when
--                       the Guard never reported power protection
--   power_reported_at   when the control plane accepted that snapshot
--
-- An absent or stale snapshot is projected as unknown, never as protected.
-- The report is bounded in code by guardpower.MaxReportBytes and Validate, so
-- this additive migration needs no constraint.
ALTER TABLE guard_posture_states
    ADD COLUMN IF NOT EXISTS power_state jsonb;

ALTER TABLE guard_posture_states
    ADD COLUMN IF NOT EXISTS power_reported_at timestamptz;
