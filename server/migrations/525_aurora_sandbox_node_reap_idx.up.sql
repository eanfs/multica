-- Scan window for the node reaper: only nodes still in an active state are
-- reaped, ordered by last_active_at. In its own file because CREATE INDEX
-- CONCURRENTLY cannot share a statement or run inside a transaction, and
-- registered in cmd/migrate/main.go's concurrentIndexCleanups so an interrupted
-- build is not mistaken for success on retry.
CREATE INDEX CONCURRENTLY aurora_sandbox_node_reap_idx ON aurora_sandbox_node (state, last_active_at) WHERE state IN ('starting', 'online', 'draining');
