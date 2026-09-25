"use client";

import { useEffect } from "react";
import {
  paths,
  setWorkspaceDestinationResolver,
  type WorkspaceDestinationContext,
} from "@multica/core/paths";
import { resolveAuroraDestination } from "@/lib/routes";

/**
 * Aurora's half of the workspace-destination contract: where this app sends a
 * user who is not (or no longer) in a workspace it can open.
 *
 * Shared core decides *when* the question is asked — a workspace deleted
 * elsewhere, the user removed from it, a URL slug that resolves to nothing —
 * and answered it with Multica-web routes (`/{slug}/issues`, `/onboarding`,
 * `/workspaces/new`) until the resolver became injectable. Aurora serves none
 * of those, so it registers its own answer here and the relocation lands
 * inside this app.
 *
 * `onboarded_at` is deliberately not consulted, for the same reason
 * `resolveAuroraDestination` ignores it: it gates the Multica questionnaire,
 * which a consumer opening a poster maker has nothing to answer. With no
 * workspace left this app serves no creation route either — registration opens
 * the personal workspace server-side — so the destination is `/login`, where
 * `NoWorkspaceNotice` explains the state and offers a log-out button; logging
 * out ends the session, and the next sign-in re-runs the workspace
 * registration.
 */
export function resolveAuroraWorkspaceDestination({
  workspaces,
}: WorkspaceDestinationContext): string {
  return resolveAuroraDestination(workspaces) ?? paths.login();
}

/**
 * Register that resolver for as long as the app shell is mounted.
 *
 * Registration also happens during render because some shared views read the
 * module-global resolver *during their own render* (`InvitePage` derives an
 * `href` from it), and React runs child effects before parent effects. An
 * effect-only registration would leave the first paint of such a child on
 * core's Multica default. The call is idempotent — it assigns the same
 * resolver — so re-renders and discarded renders are harmless.
 *
 * The effect registers too so React StrictMode's dev mount → cleanup → mount
 * cannot leave the resolver null after the cleanup. Its cleanup restores
 * core's default, so unmounting does not leave an app resolver behind in a
 * test or a second render; registration stays on the live tree rather than at
 * module load.
 */
export function useAuroraWorkspaceDestination(): void {
  if (typeof window !== "undefined") {
    setWorkspaceDestinationResolver(resolveAuroraWorkspaceDestination);
  }

  useEffect(() => {
    setWorkspaceDestinationResolver(resolveAuroraWorkspaceDestination);
    return () => setWorkspaceDestinationResolver(null);
  }, []);
}
