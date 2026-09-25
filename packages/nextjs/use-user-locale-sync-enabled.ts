"use client";

import { usePathname } from "next/navigation";
import { paths } from "@multica/core/paths";

/**
 * Whether the signed-in user's locale may be synchronized on this route.
 *
 * Both apps serve the same single-use callback route, so both have to hold the
 * reload off it. Account locale synchronization can reload the page to apply a
 * new language; the OAuth callback is a single-use flow — its code is spent on
 * the first render that exchanges it — so a reload racing that exchange strands
 * the user on a callback that can never complete. Let it finish first.
 */
export function useUserLocaleSyncEnabled(): boolean {
  const pathname = usePathname();
  return !!pathname && pathname.replace(/\/+$/, "") !== paths.authCallback();
}
