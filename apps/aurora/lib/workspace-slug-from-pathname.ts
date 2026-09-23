/**
 * The workspace slug the router is currently on, read from the live pathname.
 *
 * Mirrors apps/web/lib/workspace-slug-from-pathname.ts, which is where the
 * reasoning below comes from.
 *
 * The URL is the source of truth for workspace identity, so this is what
 * `app/[workspaceSlug]/layout.tsx` compares its own `params` slug against
 * before it writes the platform workspace singleton. `usePathname()` returns
 * one value shared by every mounted instance, which is what lets a stale layout
 * recognise that it is no longer the routed one — without it, two layouts
 * mounted side by side during a transition alternate the singleton and the
 * first child query of whichever workspace is actually on screen carries the
 * wrong `X-Workspace-Slug` header.
 *
 * Returns null for the root and for anything without a first segment. Global
 * routes (`/login`, `/auth/callback`) do return their first segment — the
 * caller is a workspace layout that only ever compares against its own slug,
 * and a reserved slug can never equal one.
 */
export function workspaceSlugFromPathname(
  pathname: string | null | undefined,
): string | null {
  const segment = pathname?.split("/").filter(Boolean)[0];
  if (!segment) return null;
  // Route params arrive decoded while the pathname segment stays encoded, so a
  // slug needing escapes would never compare equal without this. A lone `%`
  // throws rather than decoding, so fall back to the raw segment.
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}
