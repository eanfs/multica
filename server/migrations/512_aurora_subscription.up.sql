-- Aurora personal subscription: one row per user (unique index in 513).
-- tier: free | creator | pro. status: active | past_due | canceled.
-- MVP semantics: cancelation stops future grants only; already-granted
-- credits stay spendable until their monthly window expires (Plan 5 Task 4).
CREATE TABLE IF NOT EXISTS aurora_subscription (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    tier TEXT NOT NULL,
    status TEXT NOT NULL,
    stripe_customer_id TEXT,
    stripe_subscription_id TEXT,
    current_period_end timestamptz,
    cancel_at_period_end BOOLEAN NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
