-- Dropping the constraint also drops the index it was attached to, so 519's
-- down migration is a no-op.
ALTER TABLE aurora_sandbox_node DROP CONSTRAINT IF EXISTS aurora_sandbox_node_pkey;
