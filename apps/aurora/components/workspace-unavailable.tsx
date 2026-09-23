"use client";

import { useEffect } from "react";
import { useT } from "@multica/views/i18n";
import { WorkspaceRecovery } from "@/components/workspace-recovery";

/**
 * Rendered when the slug in the URL does not name a workspace this user can
 * open. Deliberately does not distinguish "no such workspace" from "exists but
 * not mine" — saying which would let anyone enumerate slugs.
 *
 * This is Aurora's own screen rather than the shared `NoAccessPage`: that one
 * recovers through `resolvePostAuthDestination`, which sends users to
 * /onboarding or /workspaces/new. Aurora serves neither, so the shared screen
 * would offer a button that 404s.
 */
export function WorkspaceUnavailable() {
  const { t } = useT("aurora");

  // Clear the stale `last_workspace_slug` cookie. The root redirect reads it
  // with no access check, so a cookie pointing at a workspace the user has just
  // lost would bounce every later visit to `/` straight back to this screen.
  useEffect(() => {
    if (typeof document === "undefined") return;
    document.cookie = "last_workspace_slug=; path=/; max-age=0; SameSite=Lax";
  }, []);

  return (
    <div className="flex h-svh flex-col items-center justify-center gap-6 px-6 text-center">
      <div className="space-y-2">
        <h1 className="text-display-sm font-semibold tracking-tight">
          {t(($) => $.workspace.unavailable_title)}
        </h1>
        <p className="max-w-md text-muted-foreground">
          {t(($) => $.workspace.unavailable_description)}
        </p>
      </div>
      <WorkspaceRecovery />
    </div>
  );
}
