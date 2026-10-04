CREATE INDEX CONCURRENTLY IF NOT EXISTS fleet_nodes_owner_idx ON fleet_nodes(namespace, owner_id, created_at, id);
