"use client";

import { useEffect } from "react";
import { useT } from "@multica/views/i18n";
import { DeadEndScreen } from "@/components/dead-end-screen";

/**
 * Rendered when the slug in the URL does not name a workspace this user can
 * open. Deliberately does not distinguish "no such workspace" from "exists but
 * not mine" — saying which would let anyone enumerate slugs.
 *
 * This is Aurora's own screen rather than the shared `NoAccessPage`, whose
 * copy describes the Multica workspace list. The recovery underneath is the
 * same decision either way: it opens the workspace the user still has, which
 * is what the shared page's resolver — now injectable, and injected here in
 * `platform/workspace-destination.ts` — would resolve to as well.
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
    <DeadEndScreen
      title={t(($) => $.workspace.unavailable_title)}
      description={t(($) => $.workspace.unavailable_description)}
    />
  );
}
