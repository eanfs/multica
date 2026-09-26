-- Dropping the constraint also drops the index it was attached to, so 528's
-- down migration is a no-op.
ALTER TABLE aurora_provider_run DROP CONSTRAINT IF EXISTS aurora_provider_run_pkey;
