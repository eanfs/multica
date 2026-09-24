-- One personal subscription per user. Aurora is a personal product line, so
-- (user_id) is the identity, not (workspace_id, user_id) — and it is the
-- conflict target of the webhook upsert, which needs a unique index to be
-- correct under concurrent deliveries of the same event.
--
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement or
-- run inside a transaction, and registered in cmd/migrate/main.go's
-- concurrentIndexCleanups so an interrupted build is not mistaken for success.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS aurora_subscription_user_idx
    ON aurora_subscription (user_id);
