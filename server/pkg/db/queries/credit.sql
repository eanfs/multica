-- name: GetCreditBalance :one
SELECT available_micro FROM credit_balance WHERE user_id = $1;

-- name: EnsureCreditBalance :exec
INSERT INTO credit_balance (user_id, available_micro) VALUES ($1, 0)
ON CONFLICT (user_id) DO NOTHING;

-- name: DeductCreditBalance :one
UPDATE credit_balance
SET available_micro = available_micro - $2, updated_at = now()
WHERE user_id = $1 AND available_micro >= $2
RETURNING available_micro;

-- name: CreditCreditBalance :one
UPDATE credit_balance
SET available_micro = available_micro + $2, updated_at = now()
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
