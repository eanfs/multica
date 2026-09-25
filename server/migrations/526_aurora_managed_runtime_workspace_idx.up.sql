-- Exactly one aurora_managed cloud runtime per workspace. This is the
-- invariant GetAuroraManagedRuntime now relies on after it stopped requiring
-- daemon_id IS NULL, so it can find the runtime whether unbound or bound to its
-- sandbox daemon. In its own file because CREATE INDEX CONCURRENTLY cannot share
-- a statement or run inside a transaction, and registered in
-- cmd/migrate/main.go's concurrentIndexCleanups so an interrupted build is not
-- mistaken for success on retry.
CREATE UNIQUE INDEX CONCURRENTLY agent_runtime_aurora_managed_workspace_uidx ON agent_runtime (workspace_id) WHERE runtime_mode = 'cloud' AND provider = 'aurora_managed';
