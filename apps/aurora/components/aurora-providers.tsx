"use client";

import { useMemo } from "react";
import { CoreProvider } from "@multica/core/platform";
import { createBrowserCookieLocaleAdapter } from "@multica/core/i18n/browser";
import type { LocaleResources, SupportedLocale } from "@multica/core/i18n";
import packageJson from "../package.json";
import { AuroraNavigationProvider } from "@/platform/navigation";
import { detectWebOS } from "@/platform/client-os";
import { useUserLocaleSyncEnabled } from "@/platform/use-user-locale-sync-enabled";
import {
  setLoggedInCookie,
  clearLoggedInCookie,
} from "@/features/auth/auth-cookie";

// Derive the WebSocket URL from the page origin so self-hosted / LAN
// deployments work without an explicit runtime wsUrl. The Next.js runtime proxy
// handles /ws -> backend when the deployment keeps WebSockets same-origin.
function deriveWsUrl(): string | undefined {
  if (typeof window === "undefined") return undefined;
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}/ws`;
}

// Build-time version preferred (CI sets NEXT_PUBLIC_APP_VERSION to a git tag or
// sha so different deploys are distinguishable in server logs); fall back to the
// package.json version so local dev still reports something useful.
const AURORA_VERSION =
  process.env.NEXT_PUBLIC_APP_VERSION || packageJson.version || "dev";

/**
 * The platform shell: one CoreProvider (api client, auth store, query client,
 * realtime socket, i18n) and the Next.js navigation adapter shared views
 * consume.
 *
 * Cookie auth is unconditional here, unlike apps/web: that app still has to
 * honour a `multica_token` in localStorage for sessions created before the
 * cookie migration, and no Aurora session predates this app.
 */
export function AuroraProviders({
  children,
  locale,
  resources,
  apiBaseUrl,
  wsUrl,
}: {
  children: React.ReactNode;
  locale: SupportedLocale;
  resources: Record<string, LocaleResources>;
  apiBaseUrl?: string;
  wsUrl?: string;
}) {
  const syncUserLocale = useUserLocaleSyncEnabled();
  // Stable identity reference so downstream effects keyed on it don't see a new
  // object on every parent render.
  const identity = useMemo(
    () => ({ platform: "web", version: AURORA_VERSION, os: detectWebOS() }),
    [],
  );
  const localeAdapter = useMemo(() => createBrowserCookieLocaleAdapter(), []);

  return (
    <CoreProvider
      apiBaseUrl={apiBaseUrl}
      wsUrl={wsUrl || deriveWsUrl()}
      cookieAuth
      onLogin={setLoggedInCookie}
      onLogout={clearLoggedInCookie}
      identity={identity}
      locale={locale}
      resources={resources}
      localeAdapter={localeAdapter}
      syncUserLocale={syncUserLocale}
    >
      <AuroraNavigationProvider>{children}</AuroraNavigationProvider>
    </CoreProvider>
  );
}
