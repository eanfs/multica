-- One personal subscription per user: the webhook upsert arbitrates on
-- (user_id), so this index is also the ON CONFLICT target.
--
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement or
-- run inside a transaction, and registered in cmd/migrate/main.go's
-- concurrentIndexCleanups so an interrupted build is not mistaken for success.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS aurora_subscription_user_idx
    ON aurora_subscription (user_id);
