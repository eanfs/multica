CREATE INDEX CONCURRENTLY IF NOT EXISTS fleet_runtime_node_idx ON agent_runtime((metadata->>'fleet_node_id'), owner_id) WHERE metadata->>'managed_by' = 'local_fleet';
