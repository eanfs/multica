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
 * the personal workspace server-side — so `/login`, where `NoWorkspaceNotice`
 * explains the state and offers the sign-in that resolves it, is the
 * destination.
 */
export function resolveAuroraWorkspaceDestination({
  workspaces,
}: WorkspaceDestinationContext): string {
  return resolveAuroraDestination(workspaces) ?? paths.login();
}

/**
 * Register that resolver for as long as the app shell is mounted.
 *
 * Mounted with the shell rather than at module load so the registration is
 * tied to a live tree: the effect is in place before the realtime socket can
 * deliver a workspace-loss event, and unmounting restores core's default
 * instead of leaving an app resolver behind in a test or a second render.
 */
export function useAuroraWorkspaceDestination(): void {
  useEffect(() => {
    setWorkspaceDestinationResolver(resolveAuroraWorkspaceDestination);
    return () => setWorkspaceDestinationResolver(null);
  }, []);
}
