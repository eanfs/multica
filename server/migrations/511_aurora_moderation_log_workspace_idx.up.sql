-- Supports the review read: WHERE workspace_id = $1 ORDER BY created_at DESC.
-- The composite (workspace_id, created_at DESC) supplies both the filter and
-- the sort, so a reviewer paging a workspace's rejections stops early instead
-- of sorting the whole log.
--
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement or
-- run inside a transaction, and registered in cmd/migrate/main.go's
-- concurrentIndexCleanups so an interrupted build is not mistaken for success.
CREATE INDEX CONCURRENTLY IF NOT EXISTS aurora_moderation_log_workspace_idx
    ON aurora_moderation_log (workspace_id, created_at DESC);
