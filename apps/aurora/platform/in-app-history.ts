/**
 * "Is there an Aurora page behind this one?" for the navigation adapter.
 *
 * Mirrors apps/web/platform/in-app-history.ts — same browser API, same answer
 * for the same reason.
 *
 * Callers that want to step back must not step back off the app when the
 * current page was opened cold — a shared link, a pasted URL, a new tab. Only
 * the browser can answer this, and exactly one API does: the Navigation API's
 * `entries()` spans just the contiguous same-origin run around the current
 * entry, so arriving from an external site reports `canGoBack` false even
 * though the browser technically has somewhere to go. `history.length` is no
 * substitute — it counts other origins' entries too, and a wrong `true` walks
 * the user out of the app, the precise failure this exists to prevent. Where
 * the browser cannot answer we report `false`, and callers take their fallback
 * path.
 */

/** Minimal shape of the Navigation API — not in TypeScript's DOM lib yet. */
type NavigationApi = { canGoBack: boolean };

/**
 * Whether a step back stays inside the app. `false` whenever the browser has no
 * Navigation API: the caller then navigates to its fallback, which is the
 * behaviour those browsers had before any of this existed.
 */
export function canGoBackInApp(): boolean {
  if (typeof window === "undefined") return false;
  const navigation = (window as { navigation?: unknown }).navigation as
    | NavigationApi
    | undefined;
  return navigation?.canGoBack === true;
}
