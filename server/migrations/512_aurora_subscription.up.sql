-- Aurora personal subscription: one row per user, reconciled from Stripe
-- webhooks (Plan 5 Task 3) and read by the local entitlement gate.
--
-- tier names the paid plan (free | creator | pro); status names the Stripe
-- lifecycle (active | past_due | canceled). Both stay TEXT rather than enums so
-- a new tier is a catalog change in application code, not a migration.
--
-- stripe_customer_id / stripe_subscription_id are nullable because a row exists
-- from the moment a user first lands on the billing page — before any Stripe
-- object does — and because a self-hosted instance runs the whole product with
-- no Stripe configured at all.
--
-- current_period_end is NULL until Stripe reports a period. It is the grant
-- window boundary (Task 4): grants run for status = 'active' rows whose period
-- has not elapsed.
--
-- cancel_at_period_end records a cancelation that has been requested but not yet
-- taken effect, so the UI can distinguish "cancels on the 3rd" from "canceled".
-- MVP semantics: cancelation stops future grants only; credits already granted
-- stay spendable until their monthly window expires (Task 4).
--
-- stripe_event_created is the `created` of the Stripe event that last wrote this
-- row, and it is what makes the upsert order-safe. Stripe does not guarantee
-- delivery order: a checkout.session.completed delayed behind a
-- customer.subscription.deleted would otherwise overwrite the cancelation and
-- reactivate the subscription — which would make the row eligible for monthly
-- grants the user is no longer paying for. The upsert refuses any event older
-- than this value. NOT NULL because every write originates from a Stripe event;
-- a caller that cannot supply one has no business writing this table.
--
-- No foreign key to user by house rule; the association is application-level.
CREATE TABLE IF NOT EXISTS aurora_subscription (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    tier TEXT NOT NULL,
    status TEXT NOT NULL,
    stripe_customer_id TEXT,
    stripe_subscription_id TEXT,
    current_period_end timestamptz,
    cancel_at_period_end BOOLEAN NOT NULL DEFAULT false,
    stripe_event_created timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
