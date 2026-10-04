ALTER TABLE IF EXISTS fleet_nodes ADD COLUMN IF NOT EXISTS spec_config jsonb NOT NULL
 DEFAULT '{"cpus":2,"memory_bytes":4294967296,"pids":256,"max_runs":1}'::jsonb;
ALTER TABLE IF EXISTS fleet_node_credentials ADD COLUMN IF NOT EXISTS generation bigint NOT NULL DEFAULT 1;
