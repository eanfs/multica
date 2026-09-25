-- Durable checkout intent and Stripe event ordering for Aurora subscriptions.
ALTER TABLE IF EXISTS aurora_subscription
    ADD COLUMN IF NOT EXISTS checkout_idempotency_key TEXT,
    ADD COLUMN IF NOT EXISTS checkout_billing_cycle TEXT,
    ADD COLUMN IF NOT EXISTS stripe_event_created_at BIGINT NOT NULL DEFAULT 0;
