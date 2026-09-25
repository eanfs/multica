import type { Workspace } from "../types";
import { useAuthStore } from "../auth";
import { paths } from "./paths";

/**
 * Priority (onboarded-first):
 *   !hasOnboarded               → /onboarding
 *   hasOnboarded + workspace[0] → /<first.slug>/issues
 *   hasOnboarded + no workspace → /workspaces/new
 *
 * V3 invariant: `onboarded_at != null` is the single source of truth for
 * "may access /<slug>/*". The web workspace layout and the desktop App.tsx
 * overlay decision both gate on this — sending an un-onboarded user
 * straight to /issues would just be redirected back to /onboarding by
 * the layout gate, costing a navigation round-trip. Check onboarded
 * first.
 *
 * In v3 "has workspace but !onboarded" is physically rare (a user can
 * only land in that state by closing the app between Step 2 and Step 3
 * — both questionnaire and runtime picker steps run after workspace
 * creation but before CompleteOnboarding). OnboardingFlow's Step 2
 * already recognizes existing workspaces and offers "Continue with
 * {name}", so the recovery is seamless.
 *
 * Callers that need invitation-aware routing (callback / login) handle
 * the "un-onboarded with pending invites" branch themselves before calling
 * this resolver — this resolver only deals with the post-invite-check
 * destination.
 *
 * This is the Multica-web answer, and the default behind
 * `resolveWorkspaceDestination`, which is what the shared surfaces call.
 * Keep app-specific post-auth flows (apps/web login/onboarding/callback) on
 * this function: they are routing inside the app that owns these routes.
 */
export function resolvePostAuthDestination(
  workspaces: Workspace[],
  hasOnboarded: boolean,
): string {
  if (!hasOnboarded) {
    return paths.onboarding();
  }
  const first = workspaces[0];
  if (first) {
    return paths.workspace(first.slug).issues();
  }
  return paths.newWorkspace();
}

/**
 * The inputs every app's destination decision is made from. Deliberately the
 * same two facts `resolvePostAuthDestination` reads, so an app can inject a
 * resolver that only changes the routes without changing the reasoning.
 */
export interface WorkspaceDestinationContext {
  /**
   * Workspaces the user still has access to, with the lost one already
   * removed. Empty is a real state — an account can end up with no workspace.
   */
  workspaces: Workspace[];
  /** Whether the user has completed the Multica onboarding questionnaire. */
  hasOnboarded: boolean;
}

/**
 * Where an app sends a user who is not (or no longer) in a workspace it can
 * open. Must return a route the injecting app actually serves: the caller
 * navigates there directly, so a route from another app's space is a 404.
 */
export type WorkspaceDestinationResolver = (
  context: WorkspaceDestinationContext,
) => string;

// Module-level, like `setCurrentWorkspace` and the system-notification click
// handler: the callers live in `packages/core` (realtime) and `packages/views`
// (workspace tab, dashboard guard, no-access page, invite page), and neither
// can be reached by a React context provided from the app layer.
let workspaceDestinationResolver: WorkspaceDestinationResolver | null = null;

/**
 * Inject how this app answers "where does a user who lost their workspace
 * go?" — a destination it serves, instead of the Multica-web routes the
 * default resolver returns. Call it from the app shell at startup; pass `null`
 * to go back to the default.
 *
 * Mirrors the NavigationAdapter pattern: the shared layer owns when the
 * decision is made, the app owns what the answer is.
 */
export function setWorkspaceDestinationResolver(
  resolver: WorkspaceDestinationResolver | null,
): void {
  workspaceDestinationResolver = resolver;
}

/**
 * Resolve where the shared surfaces send a user who is not (or no longer) in a
 * workspace they can open — after the current workspace is deleted elsewhere
 * or they are removed from it (realtime), after they leave or delete it, when
 * the URL slug does not resolve, and as the invite page's fallback.
 *
 * Without an injected resolver this is `resolvePostAuthDestination`, so web
 * and desktop are unchanged. Apps that serve a different route space (Aurora)
 * inject their own via `setWorkspaceDestinationResolver`.
 */
export function resolveWorkspaceDestination(
  context: WorkspaceDestinationContext,
): string {
  if (workspaceDestinationResolver) {
    return workspaceDestinationResolver(context);
  }
  return resolvePostAuthDestination(context.workspaces, context.hasOnboarded);
}

/**
 * Single source of truth: backed by `users.onboarded_at`, which
 * arrives with the user object on every auth response.
 */
export function useHasOnboarded(): boolean {
  return useAuthStore((s) => s.user?.onboarded_at != null);
}
