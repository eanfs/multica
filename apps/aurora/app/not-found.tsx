"use client";

import { useT } from "@multica/views/i18n";
import { DeadEndScreen } from "@/components/dead-end-screen";

/**
 * Any URL this app does not serve.
 *
 * This is not only for typos. Shared core navigates away from a workspace that
 * disappeared with a full-page `window.location.assign` to a Multica route —
 * `/{slug}/issues`, `/onboarding`, `/workspaces/new` (`resolvePostAuthDestination`
 * in `packages/core/paths/resolve.ts`, called from `realtime/use-realtime-sync.ts`
 * when the current workspace is deleted elsewhere or the user is removed from
 * it). Aurora serves none of those, so without this screen the user is dropped
 * on Next.js's default 404 with no way back. Sending the core relocation
 * somewhere else means giving it an app-supplied destination, which is a core
 * change; until then this catches it and offers the way out.
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
