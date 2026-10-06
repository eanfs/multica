CREATE INDEX CONCURRENTLY IF NOT EXISTS fleet_operations_pending_idx ON fleet_node_operations(namespace, owner_id, phase, created_at, id) WHERE phase NOT IN ('completed', 'failed');
