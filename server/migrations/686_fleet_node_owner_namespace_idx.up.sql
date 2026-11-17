CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fleet_node_owner_namespace ON fleet_node (owner_id, namespace);
