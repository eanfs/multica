CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_operations_idempotency_idx ON fleet_node_operations(namespace, owner_id, idempotency_key);
