-- Aurora credit wallet: one row per user. available_micro is the spendable
-- micro-credit balance (1 credit = 1e6 micro, 1 USD = 1000 credit).
CREATE TABLE IF NOT EXISTS credit_balance (
    user_id UUID PRIMARY KEY,
    available_micro BIGINT NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);
