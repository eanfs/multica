-- Scan window for the monthly grant cron (Plan 5 Task 4): the settlement
-- reads every active subscription whose current period has not ended.
--
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement or
-- run inside a transaction, and registered in cmd/migrate/main.go's
-- concurrentIndexCleanups so an interrupted build is not mistaken for success.
CREATE INDEX CONCURRENTLY IF NOT EXISTS aurora_subscription_status_period_idx
    ON aurora_subscription (status, current_period_end);
