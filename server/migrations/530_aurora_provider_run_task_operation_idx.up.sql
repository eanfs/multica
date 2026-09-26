-- One create lease per (task_id, operation): the write-time arbiter that makes
-- a duplicate begin lose the insert instead of creating a second billable run.
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement or
-- run inside a transaction, and registered in cmd/migrate/main.go's
-- concurrentIndexCleanups so an interrupted build is not mistaken for success on
-- retry.
CREATE UNIQUE INDEX CONCURRENTLY aurora_provider_run_task_operation_uidx
    ON aurora_provider_run (task_id, operation);
