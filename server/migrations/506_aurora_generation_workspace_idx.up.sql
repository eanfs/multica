-- Supports ListAuroraGenerations: WHERE workspace_id = $1 ORDER BY created_at
-- DESC LIMIT $2. The composite (workspace_id, created_at DESC) supplies both the
-- filter and the sort so the reader's LIMIT stops the scan early instead of
-- sorting the whole workspace's generations on every page load.
--
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement
-- or run inside a transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS aurora_generation_workspace_idx
    ON aurora_generation (workspace_id, created_at DESC);
