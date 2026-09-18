-- Supports ListCreditTransactions: WHERE user_id = $1 ORDER BY created_at DESC
-- LIMIT $2. Without it every billing page load is a sequential scan of the
-- user's whole ledger plus a sort — and the ledger is append-only and never
-- pruned (workspace teardown detaches rows rather than deleting them), so the
-- cost grows without bound. Column order is (user_id, created_at DESC) so the
-- index supplies both the filter and the sort; the reader's LIMIT then stops
-- the scan early instead of materializing every row.
--
-- In its own file because CREATE INDEX CONCURRENTLY cannot share a statement
-- or run inside a transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS credit_ledger_user_created_at_idx
    ON credit_ledger (user_id, created_at DESC);
