-- Dropping the constraint also drops the index it was attached to, so 533's
-- down migration is a no-op.
ALTER TABLE aurora_artifact_staging DROP CONSTRAINT IF EXISTS aurora_artifact_staging_pkey;
