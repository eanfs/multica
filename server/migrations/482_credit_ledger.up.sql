-- Append-only credit ledger. kind follows the cloud wallet contract
-- (packages/core/types/billing.ts): topup | deduction | refund | adjustment
-- | expire (expire is implemented by Plan 5's monthly settlement).
-- amount_micro is signed (deduction/expire negative, others positive).
-- reference names the operation's subject — a generation id, a Stripe event
-- id, or a Plan 5 grant key ("sub:<userID>:<YYYY-MM>", "signup:<userID>",
-- "<userID>:<YYYY-MM>") — so the transactions UI can label each row; the
-- UI falls back to kind-based labels for non-generation references.
-- idempotency_key makes retries safe; the unique index enforcing it lives
-- in its own migration file.
CREATE TABLE IF NOT EXISTS credit_ledger (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    kind TEXT NOT NULL,
    amount_micro BIGINT NOT NULL,
    balance_after_micro BIGINT NOT NULL,
    reference TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
