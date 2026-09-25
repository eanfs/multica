-- name: StartAuroraSubscriptionCheckout :one
-- Persist the checkout intent before calling Stripe. Reusing the stored key makes
-- retries and simultaneous tabs converge on one Stripe Checkout Session.
INSERT INTO aurora_subscription (
    user_id, tier, status, stripe_customer_id, stripe_subscription_id,
    current_period_end, cancel_at_period_end, checkout_idempotency_key,
    checkout_billing_cycle, stripe_event_created_at
)
VALUES (
    sqlc.arg(user_id), sqlc.arg(tier), 'pending', NULL, NULL,
    NULL, false, sqlc.arg(checkout_idempotency_key),
    sqlc.arg(checkout_billing_cycle), 0
)
ON CONFLICT (user_id) DO UPDATE SET
    tier = EXCLUDED.tier,
    status = 'pending',
    stripe_customer_id = NULL,
    stripe_subscription_id = NULL,
    current_period_end = NULL,
    cancel_at_period_end = false,
    checkout_idempotency_key = EXCLUDED.checkout_idempotency_key,
    checkout_billing_cycle = EXCLUDED.checkout_billing_cycle,
    stripe_event_created_at = 0,
    updated_at = now()
RETURNING id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
          current_period_end, cancel_at_period_end, created_at, updated_at,
          checkout_idempotency_key, checkout_billing_cycle, stripe_event_created_at;

-- name: UpsertAuroraSubscription :one
-- Apply only a non-stale Stripe event. Event timestamps have one-second
-- precision, so a canceled subscription also wins ties: that Stripe
-- subscription cannot become active again, and a late same-second update must
-- not resurrect it.
INSERT INTO aurora_subscription (
    user_id, tier, status, stripe_customer_id, stripe_subscription_id,
    current_period_end, cancel_at_period_end, stripe_event_created_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (user_id) DO UPDATE SET
    tier = EXCLUDED.tier,
    status = EXCLUDED.status,
    stripe_customer_id = COALESCE(EXCLUDED.stripe_customer_id, aurora_subscription.stripe_customer_id),
    stripe_subscription_id = COALESCE(EXCLUDED.stripe_subscription_id, aurora_subscription.stripe_subscription_id),
    current_period_end = EXCLUDED.current_period_end,
    cancel_at_period_end = EXCLUDED.cancel_at_period_end,
    checkout_idempotency_key = NULL,
    checkout_billing_cycle = NULL,
    stripe_event_created_at = EXCLUDED.stripe_event_created_at,
    updated_at = now()
WHERE aurora_subscription.stripe_event_created_at < EXCLUDED.stripe_event_created_at
   OR (
       aurora_subscription.stripe_event_created_at = EXCLUDED.stripe_event_created_at
       AND NOT (
           aurora_subscription.status = 'canceled'
           AND EXCLUDED.status <> 'canceled'
       )
   )
RETURNING id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
          current_period_end, cancel_at_period_end, created_at, updated_at,
          checkout_idempotency_key, checkout_billing_cycle, stripe_event_created_at;

-- name: GetAuroraSubscriptionByUser :one
SELECT id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
       current_period_end, cancel_at_period_end, created_at, updated_at,
       checkout_idempotency_key, checkout_billing_cycle, stripe_event_created_at
FROM aurora_subscription
WHERE user_id = $1;

-- name: GetAuroraSubscriptionByStripeID :one
SELECT id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
       current_period_end, cancel_at_period_end, created_at, updated_at,
       checkout_idempotency_key, checkout_billing_cycle, stripe_event_created_at
FROM aurora_subscription
WHERE stripe_subscription_id = $1;

-- name: ListActiveSubscriptionsForGrant :many
SELECT id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
       current_period_end, cancel_at_period_end, created_at, updated_at,
       checkout_idempotency_key, checkout_billing_cycle, stripe_event_created_at
FROM aurora_subscription
WHERE status = 'active' AND current_period_end > now()
ORDER BY user_id;
