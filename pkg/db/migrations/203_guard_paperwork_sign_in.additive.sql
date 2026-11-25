SET LOCAL lock_timeout = '5s';

ALTER TABLE guard_reach_states
    ADD COLUMN IF NOT EXISTS paperwork_sign_in jsonb NOT NULL DEFAULT '{}'::jsonb;
