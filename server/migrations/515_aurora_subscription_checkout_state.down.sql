ALTER TABLE IF EXISTS aurora_subscription
    DROP COLUMN IF EXISTS stripe_event_created_at,
    DROP COLUMN IF EXISTS checkout_billing_cycle,
    DROP COLUMN IF EXISTS checkout_idempotency_key;
