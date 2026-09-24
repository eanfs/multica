-- Scan window for the monthly grant cron (Task 4): it selects status = 'active'
-- rows whose current_period_end has elapsed, so the composite supplies both the
-- equality filter and the range predicate without touching the table.
--
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement or
-- run inside a transaction, and registered in cmd/migrate/main.go's
-- concurrentIndexCleanups so an interrupted build is not mistaken for success.
CREATE INDEX CONCURRENTLY IF NOT EXISTS aurora_subscription_status_period_idx
    ON aurora_subscription (status, current_period_end);
