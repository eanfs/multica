-- Supports ListAuroraAssets filtered by generation_id: the generation-detail
-- asset list looks up every asset produced by one generation.
--
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement
-- or run inside a transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS aurora_asset_generation_idx
    ON aurora_asset (generation_id);
