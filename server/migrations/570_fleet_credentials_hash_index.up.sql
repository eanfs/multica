CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_credentials_hash_idx ON fleet_node_credentials(token_hash);
