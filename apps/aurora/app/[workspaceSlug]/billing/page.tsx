import { AuroraBilling } from "@multica/views/aurora";

/**
 * Plan, balance and credit activity.
 *
 * Checkout is started from the page itself: the view builds the Stripe return
 * URLs from the navigation adapter's app origin and this route's workspace
 * slug, so the app layer has no route to wire up.
 */
export default function BillingPage() {
  return <AuroraBilling />;
}
