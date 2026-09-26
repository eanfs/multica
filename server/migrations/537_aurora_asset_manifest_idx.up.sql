-- At most one asset per (generation, manifest artifact id), so a replayed
-- artifact report cannot duplicate a workspace asset. Partial because the
-- pre-Plan-C writeback rows have no manifest artifact id. In its own file
-- because CREATE INDEX CONCURRENTLY cannot share a statement or run inside a
-- transaction, and registered in cmd/migrate/main.go's concurrentIndexCleanups.
CREATE UNIQUE INDEX CONCURRENTLY aurora_asset_generation_manifest_uidx
    ON aurora_asset (generation_id, manifest_artifact_id) WHERE manifest_artifact_id IS NOT NULL;
