-- Exactly one sandbox node per daemon identity. In its own file because CREATE
-- INDEX CONCURRENTLY cannot share a statement or run inside a transaction, and
-- registered in cmd/migrate/main.go's concurrentIndexCleanups so an interrupted
-- build is not mistaken for success on retry.
CREATE UNIQUE INDEX CONCURRENTLY aurora_sandbox_node_daemon_uidx ON aurora_sandbox_node (daemon_id);
