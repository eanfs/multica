import { NextResponse, type NextRequest } from "next/server";
import { LOCALE_COOKIE } from "@multica/core/i18n";
import { isReservedSlug } from "@multica/core/paths";
import {
  MULTICA_LOCALE_HEADER,
  resolveLocaleFromSignals,
} from "./lib/locale-routing";
import {
  isBackendSurfacePath,
  runtimeRewriteDestination,
} from "./config/runtime-urls";

/**
 * Route segments this app owns at the root. Every other reserved slug names a
 * global Multica route — `/onboarding`, `/workspaces/new` — and a workspace can
 * never be named one (creation rejects reserved slugs).
 *
 * Redirecting them matters beyond stray URLs. Shared core relocates away from a
 * lost workspace with a full-page `window.location.assign` to one of those
 * paths (`resolvePostAuthDestination` in `packages/core/paths/resolve.ts`,
 * called from `realtime/use-realtime-sync.ts` when the current workspace is
 * deleted elsewhere or the user is removed from it). Aurora serves none of
 * them, and an Aurora-only account has `onboarded_at == null` — the
 * questionnaire is a Multica-web flow — so the `/onboarding` branch is the
 * common one. The root resolves to the workspace the user still has.
 */
const APP_ROOT_SEGMENTS = new Set(["login", "auth"]);

/**
 * Two jobs, both of which have to happen before a route renders.
 *
 * 1. Send backend traffic to the configured API origin. `next dev` does this
 *    with a rewrite in next.config.ts; a prebuilt image has no upstream baked
 *    in, so it resolves REMOTE_API_URL per request instead.
 * 2. Stamp the resolved locale onto the request so the root layout renders the
 *    right resources on the server, which is what keeps the first paint from
 *    mismatching the client.
 *
 * Next.js 16 renamed `middleware` to `proxy`. The API surface (NextRequest /
 * NextResponse / cookies / matcher) is identical; the behavioral change is the
 * runtime — proxy is forced to nodejs and cannot opt into edge.
 */
function resolveLocale(req: NextRequest): string {
  return resolveLocaleFromSignals({
    cookieLocale: req.cookies.get(LOCALE_COOKIE)?.value,
    acceptLanguage: req.headers.get("accept-language"),
  });
}

// The `request: { headers }` form is what makes the header land on the upstream
// request; without it the value would only sit on the response.
function nextWithLocale(req: NextRequest): NextResponse {
  const headers = new Headers(req.headers);
  headers.set(MULTICA_LOCALE_HEADER, resolveLocale(req));
  return NextResponse.next({ request: { headers } });
}

export function proxy(req: NextRequest) {
  const { pathname } = req.nextUrl;
  const runtimeDestination = runtimeRewriteDestination(pathname, process.env);
  if (runtimeDestination) {
    const url = new URL(runtimeDestination);
    url.search = req.nextUrl.search;
    return NextResponse.rewrite(url);
  }

  const firstSegment = pathname.split("/")[1] ?? "";
  // Backend paths are exempt: `api`, `v1`, `ws`, `health` and `uploads` are
  // reserved slugs, so a rule that only saw the first segment would answer the
  // API, uploads and the realtime handshake with a 307 to the app root. They
  // fall through to the rewrites that own them — this proxy's own when an
  // origin is configured, next.config.ts's dev fallback when none is.
  if (
    !isBackendSurfacePath(pathname) &&
    isReservedSlug(firstSegment) &&
    !APP_ROOT_SEGMENTS.has(firstSegment)
  ) {
    const url = req.nextUrl.clone();
    url.pathname = "/";
    url.search = "";
    return NextResponse.redirect(url);
  }

  return nextWithLocale(req);
}

export const config = {
  // The locale header must land on every page request, so this uses the
  // standard negative-lookahead pattern from Next's i18n guide, plus the
  // runtime proxy routes whose upstream origin is resolved from process.env at
  // request time instead of being baked into the config at build time.
  matcher: [
    "/v1/:path*",
    "/api/:path*",
    "/auth/:path*",
    "/uploads/:path*",
    "/ws",
    "/((?!api|v1|_next/static|_next/image|favicon.ico|.*\\.).*)",
  ],
};
