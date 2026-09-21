-- Supports the settlement reverse lookup GetAuroraGenerationByTaskID
-- (WHERE task_id = $1), which runs on every terminal agent task to decide
-- whether it backed an Aurora generation. Without this index that probe is a
-- sequential scan of the whole table on every task completion/failure.
--
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement
-- or run inside a transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS aurora_generation_task_idx
    ON aurora_generation (task_id);
