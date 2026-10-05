ALTER TABLE IF EXISTS fleet_node_operations DROP COLUMN IF EXISTS next_attempt_at;
ALTER TABLE IF EXISTS fleet_node_operations DROP COLUMN IF EXISTS non_retryable;
ALTER TABLE IF EXISTS fleet_node_operations DROP COLUMN IF EXISTS bootstrap_minted;
ALTER TABLE IF EXISTS fleet_node_operations DROP COLUMN IF EXISTS bootstrap_claimed_at;
ALTER TABLE IF EXISTS fleet_nodes DROP COLUMN IF EXISTS observation;
