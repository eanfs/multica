-- Idempotency key uniqueness, in its own file because CREATE UNIQUE INDEX
-- CONCURRENTLY cannot share a statement or run inside a transaction.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS credit_ledger_idempotency_key_idx
    ON credit_ledger (idempotency_key);
