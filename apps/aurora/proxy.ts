import { NextResponse, type NextRequest } from "next/server";
import { LOCALE_COOKIE } from "@multica/core/i18n";
import {
  MULTICA_LOCALE_HEADER,
  resolveLocaleFromSignals,
} from "./lib/locale-routing";
import { runtimeRewriteDestination } from "./config/runtime-urls";

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
