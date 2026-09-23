import { AuroraBilling } from "@multica/views/aurora";

/**
 * Balance and credit activity.
 *
 * `topUpHref` is withheld for the same reason as on the directory page: the
 * checkout route arrives with Plan 5. Without it the entry renders disabled
 * with the reason, which is the honest state until it exists.
 */
export default function BillingPage() {
  return <AuroraBilling />;
}
