"use client";

import { useT } from "@multica/views/i18n";
import { DeadEndScreen } from "@/components/dead-end-screen";

/**
 * Any URL this app does not serve.
 *
 * A net, not the mechanism: the URLs core used to send a workspace-losing user
 * to — `/{slug}/issues`, `/onboarding`, `/workspaces/new` — are no longer
 * among them, because the app registers where that relocation should land
 * (`platform/workspace-destination.ts`). What is left for this screen is
 * everything else that does not name a route here: a typo, a stale link, a
 * path from another app. Without it those users get Next.js's default 404 with
 * no way back.
 */
export default function NotFound() {
  const { t } = useT("common");

  return (
    <DeadEndScreen
      title={t(($) => $.not_found.title)}
      description={t(($) => $.not_found.description)}
    />
  );
}
