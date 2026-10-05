-- Existing rows remain unknown; this is not maintenance approval or secret storage.
ALTER TABLE IF EXISTS fleet_nodes ADD COLUMN IF NOT EXISTS observation jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE IF EXISTS fleet_node_operations ADD COLUMN IF NOT EXISTS bootstrap_claimed_at timestamptz;
ALTER TABLE IF EXISTS fleet_node_operations ADD COLUMN IF NOT EXISTS bootstrap_minted boolean NOT NULL DEFAULT false;
ALTER TABLE IF EXISTS fleet_node_operations ADD COLUMN IF NOT EXISTS non_retryable boolean NOT NULL DEFAULT false;
ALTER TABLE IF EXISTS fleet_node_operations ADD COLUMN IF NOT EXISTS next_attempt_at timestamptz;
