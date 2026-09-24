-- name: UpsertAuroraSubscription :one
-- One personal subscription per user, so the conflict target is user_id (the
-- unique index from migration 513). Nullable Stripe ids keep their previous
-- value when the incoming event does not carry one: a subscription.updated
-- event without an expanded customer must not erase the customer id the
-- checkout session already stored.
INSERT INTO aurora_subscription (user_id, tier, status, stripe_customer_id, stripe_subscription_id, current_period_end, cancel_at_period_end)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id) DO UPDATE SET
    tier = EXCLUDED.tier,
    status = EXCLUDED.status,
    stripe_customer_id = COALESCE(EXCLUDED.stripe_customer_id, aurora_subscription.stripe_customer_id),
    stripe_subscription_id = COALESCE(EXCLUDED.stripe_subscription_id, aurora_subscription.stripe_subscription_id),
    current_period_end = EXCLUDED.current_period_end,
    cancel_at_period_end = EXCLUDED.cancel_at_period_end,
    updated_at = now()
RETURNING id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
          current_period_end, cancel_at_period_end, created_at, updated_at;

-- name: GetAuroraSubscriptionByUser :one
SELECT id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
       current_period_end, cancel_at_period_end, created_at, updated_at
FROM aurora_subscription
WHERE user_id = $1;

-- name: GetAuroraSubscriptionByStripeID :one
SELECT id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
       current_period_end, cancel_at_period_end, created_at, updated_at
FROM aurora_subscription
WHERE stripe_subscription_id = $1;

-- name: ListActiveSubscriptionsForGrant :many
SELECT id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
       current_period_end, cancel_at_period_end, created_at, updated_at
FROM aurora_subscription
WHERE status = 'active' AND current_period_end > now()
ORDER BY user_id;
