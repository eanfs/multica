-- name: GetCreditBalance :one
SELECT available_micro FROM credit_balance WHERE user_id = $1;

-- name: EnsureCreditBalance :exec
INSERT INTO credit_balance (user_id, available_micro) VALUES ($1, 0)
ON CONFLICT (user_id) DO NOTHING;

-- name: DeductCreditBalance :one
UPDATE credit_balance
SET available_micro = available_micro - sqlc.arg('amount_micro'), updated_at = now()
WHERE user_id = $1 AND available_micro >= sqlc.arg('amount_micro')
RETURNING available_micro;

-- name: CreditCreditBalance :one
UPDATE credit_balance
SET available_micro = available_micro + sqlc.arg('amount_micro'), updated_at = now()
WHERE user_id = $1
RETURNING available_micro;

-- name: GetCreditLedgerByIdempotencyKey :one
SELECT id FROM credit_ledger WHERE idempotency_key = $1;

-- name: InsertCreditLedger :one
INSERT INTO credit_ledger (user_id, workspace_id, kind, amount_micro, balance_after_micro, reference, idempotency_key)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING id;

-- name: ListCreditTransactions :many
SELECT id, user_id, workspace_id, kind, amount_micro, balance_after_micro, reference, idempotency_key, created_at
FROM credit_ledger
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: SumCreditLedgerInWindow :one
-- Net signed movement in a window, for the monthly expiry (Plan 5 Task 4).
-- Deductions are negative and refunds positive in the ledger, so a plain SUM
-- is already the net spend; kinds is passed as an array so one query serves
-- both the spend window and any future narrower one.
SELECT coalesce(sum(amount_micro), 0)::bigint
FROM credit_ledger
WHERE user_id = sqlc.arg(user_id) AND kind = ANY(sqlc.arg(kinds)::text[])
  AND created_at >= sqlc.arg(from_ts) AND created_at < sqlc.arg(to_ts);

-- name: SumMonthlyGrantInWindow :one
-- Monthly "sub:" grants only — signup bonuses and topups never expire, so the
-- expiry phase must not sum them (Plan 5 Task 4).
SELECT coalesce(sum(amount_micro), 0)::bigint
FROM credit_ledger
WHERE user_id = sqlc.arg(user_id) AND kind = 'adjustment'
  AND reference LIKE 'sub:%'
  AND created_at >= sqlc.arg(from_ts) AND created_at < sqlc.arg(to_ts);

-- name: ListMonthlyGrantRecipients :many
-- Every user who received a monthly grant in the window: the expiry phase's
-- work list (Plan 5 Task 4).
SELECT DISTINCT user_id FROM credit_ledger
WHERE kind = 'adjustment' AND reference LIKE 'sub:%'
  AND created_at >= sqlc.arg(from_ts) AND created_at < sqlc.arg(to_ts);
