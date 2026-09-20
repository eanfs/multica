-- Supports ListAuroraAssets filtered by workspace_id and ordered by created_at
-- DESC: the workspace-wide asset library page. Column order is (workspace_id,
-- created_at DESC) so the index supplies both the filter and the sort.
--
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement
-- or run inside a transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS aurora_asset_workspace_idx
    ON aurora_asset (workspace_id, created_at DESC);
