"use client";

import { Suspense, useSyncExternalStore } from "react";
import { useRouter, usePathname, useSearchParams } from "next/navigation";
import {
  NavigationProvider,
  type NavigationAdapter,
} from "@multica/views/navigation";
import { canGoBackInApp } from "./in-app-history";

/**
 * Aurora's half of the shared navigation contract — the same adaptation of
 * Next.js routing that apps/web/platform/navigation.tsx performs, minus the
 * `multica:navigate` bridge (nothing reachable from Aurora's three pages fires
 * that event) and the `openInNewTab` slot (desktop only).
 *
 * The fragment is client-only state Next.js never surfaces: `usePathname()`
 * drops it, and a `router.replace("/x#y")` mutates `window.location` without a
 * render of its own. Reading it through an external store re-reads the URL on
 * every render and re-renders on the events that change it behind React's back,
 * so `adapter.hash` is never a stale copy.
 */
function subscribeToHash(onStoreChange: () => void): () => void {
  window.addEventListener("hashchange", onStoreChange);
  window.addEventListener("popstate", onStoreChange);
  return () => {
    window.removeEventListener("hashchange", onStoreChange);
    window.removeEventListener("popstate", onStoreChange);
  };
}

function NavigationProviderInner({
  children,
}: {
  children: React.ReactNode;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const hash = useSyncExternalStore(
    subscribeToHash,
    () => window.location.hash,
    () => "",
  );

  const adapter: NavigationAdapter = {
    push: router.push,
    replace: router.replace,
    back: router.back,
    forward: router.forward,
    canGoBack: canGoBackInApp,
    pathname,
    searchParams: new URLSearchParams(searchParams.toString()),
    hash,
    getShareableUrl: (path: string) =>
      typeof window === "undefined" ? path : window.location.origin + path,
    // router.prefetch is a no-op in dev mode by Next.js design; in production it
    // warms the RSC payload + route chunk so the next push() commits with no
    // network round-trip. Safe to call repeatedly — Next dedupes internally.
    prefetch: (path: string) => {
      router.prefetch(path);
    },
  };

  return <NavigationProvider value={adapter}>{children}</NavigationProvider>;
}

export function AuroraNavigationProvider({
  children,
}: {
  children: React.ReactNode;
}) {
  // useSearchParams needs a Suspense boundary in the App Router.
  return (
    <Suspense>
      <NavigationProviderInner>{children}</NavigationProviderInner>
    </Suspense>
  );
}
