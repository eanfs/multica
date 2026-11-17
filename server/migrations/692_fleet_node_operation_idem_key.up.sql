CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_node_operation_idempotency_key_key ON fleet_node_operation (idempotency_key) WHERE idempotency_key <> '';
