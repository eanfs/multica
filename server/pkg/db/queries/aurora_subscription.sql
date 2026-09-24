-- name: UpsertAuroraSubscription :one
-- The webhook is the only writer. user_id is the conflict target (unique index
-- aurora_subscription_user_idx), so replaying an event converges on one row.
-- The Stripe ids COALESCE rather than overwrite: an event that carries no
-- subscription id (a customer.created, say) must not erase the id a later
-- lookup depends on.
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
-- Entitlement resolution: a user with no row is on the free tier, so the caller
-- treats pgx.ErrNoRows as "free", not as an error.
SELECT id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
       current_period_end, cancel_at_period_end, created_at, updated_at
FROM aurora_subscription
WHERE user_id = $1;

-- name: GetAuroraSubscriptionByStripeID :one
-- Webhook routing: the event names a Stripe subscription, and this maps it back
-- to the local user.
SELECT id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
       current_period_end, cancel_at_period_end, created_at, updated_at
FROM aurora_subscription
WHERE stripe_subscription_id = $1;

-- name: ListActiveSubscriptionsForGrant :many
-- Monthly grant scan (Task 4). Only rows whose period has not elapsed are
-- eligible: a lapsed period means the subscription stopped being paid for, and
-- granting from it would hand out credits Stripe never charged for.
SELECT id, user_id, tier, status, stripe_customer_id, stripe_subscription_id,
       current_period_end, cancel_at_period_end, created_at, updated_at
FROM aurora_subscription
WHERE status = 'active' AND current_period_end > now()
ORDER BY user_id;
