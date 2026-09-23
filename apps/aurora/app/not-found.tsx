"use client";

import { useT } from "@multica/views/i18n";
import { WorkspaceRecovery } from "@/components/workspace-recovery";

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
    <div className="flex min-h-svh flex-col items-center justify-center gap-6 px-6 text-center">
      <div className="space-y-2">
        <h1 className="text-display-sm font-semibold tracking-tight">
          {t(($) => $.not_found.title)}
        </h1>
        <p className="max-w-md text-muted-foreground">
          {t(($) => $.not_found.description)}
        </p>
      </div>
      <WorkspaceRecovery />
    </div>
  );
}
