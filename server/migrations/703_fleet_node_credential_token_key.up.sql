CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_node_credential_token_hash_key ON fleet_node_credential (token_hash);
