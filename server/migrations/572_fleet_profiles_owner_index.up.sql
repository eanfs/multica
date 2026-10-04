CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_profiles_owner_idx ON fleet_credential_profiles(namespace, owner_id);
