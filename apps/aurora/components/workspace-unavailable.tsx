"use client";

import { useEffect } from "react";
import { Button } from "@multica/ui/components/ui/button";
import { useWorkspaceList } from "@multica/core/workspace";
import { useNavigation } from "@multica/views/navigation";
import { useT } from "@multica/views/i18n";
import { useLogout } from "@multica/views/auth";
import { auroraRoutes } from "@/lib/routes";

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
  const { t: tLayout } = useT("layout");
  const { replace } = useNavigation();
  const logout = useLogout();
  const { workspaces } = useWorkspaceList();
  const first = workspaces[0];

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
      <div className="flex flex-col gap-2 sm:flex-row">
        {/* Absent, not disabled, when the list holds nothing to open: the
            account has no destination and the only way forward is signing in
            again, which the second button already offers. */}
        {first ? (
          <Button
            type="button"
            onClick={() => replace(auroraRoutes(first.slug).skills())}
          >
            {t(($) => $.workspace.open_workspace)}
          </Button>
        ) : null}
        <Button type="button" variant="outline" onClick={logout}>
          {tLayout(($) => $.sidebar.log_out)}
        </Button>
      </div>
    </div>
  );
}
