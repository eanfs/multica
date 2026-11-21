CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fleet_node_operation_node_created ON fleet_node_operation (node_id, created_at DESC);
