import { describe, expect, it } from "vitest";
import { NextRequest } from "next/server";
import { LOCALE_COOKIE } from "@multica/core/i18n";
import { MULTICA_LOCALE_HEADER } from "./lib/locale-routing";
import { config, proxy } from "./proxy";

function makeRequest(
  path: string,
  cookies: Record<string, string> = {},
  headers: Record<string, string> = {},
) {
  const cookieHeader = Object.entries(cookies)
    .map(([key, value]) => `${key}=${value}`)
    .join("; ");

  return new NextRequest(`https://aurora.test${path}`, {
    headers: {
      ...(cookieHeader ? { cookie: cookieHeader } : {}),
      ...headers,
    },
  });
}

function restoreEnv(key: string, value: string | undefined) {
  if (value === undefined) delete process.env[key];
  else process.env[key] = value;
}

/** Run `body` with no API upstream configured, then put the env back. */
function withoutRuntimeUpstream(run: () => void) {
  const previousRemoteApiUrl = process.env.REMOTE_API_URL;
  const previousPublicApiUrl = process.env.NEXT_PUBLIC_API_URL;
  delete process.env.REMOTE_API_URL;
  delete process.env.NEXT_PUBLIC_API_URL;

  try {
    run();
  } finally {
    restoreEnv("REMOTE_API_URL", previousRemoteApiUrl);
    restoreEnv("NEXT_PUBLIC_API_URL", previousPublicApiUrl);
  }
}

describe("proxy runtime rewrites", () => {
  it("sends backend paths to the configured API origin", () => {
    const previous = process.env.REMOTE_API_URL;
    process.env.REMOTE_API_URL = "https://api.aurora.test";
    try {
      const response = proxy(makeRequest("/api/aurora/skills?limit=5"));
      expect(response.headers.get("x-middleware-rewrite")).toBe(
        "https://api.aurora.test/api/aurora/skills?limit=5",
      );
    } finally {
      restoreEnv("REMOTE_API_URL", previous);
    }
  });

  it("leaves the app's own OAuth callback alone", () => {
    const previous = process.env.REMOTE_API_URL;
    process.env.REMOTE_API_URL = "https://api.aurora.test";
    try {
      // The browser has to reach this app's route, not the backend's handler.
      const response = proxy(makeRequest("/auth/callback?code=abc"));
      expect(response.headers.get("x-middleware-rewrite")).toBeNull();
    } finally {
      restoreEnv("REMOTE_API_URL", previous);
    }
  });

  it("proxies nothing when no upstream is configured", () => {
    withoutRuntimeUpstream(() => {
      expect(
        proxy(makeRequest("/api/aurora/skills")).headers.get(
          "x-middleware-rewrite",
        ),
      ).toBeNull();
    });
  });
});

describe("proxy reserved-slug redirects", () => {
  it("sends global Multica routes this app does not serve to the root", () => {
    // Shared core relocates a lost workspace to exactly these paths.
    expect(proxy(makeRequest("/onboarding")).headers.get("location")).toBe(
      "https://aurora.test/",
    );
    expect(proxy(makeRequest("/workspaces/new")).headers.get("location")).toBe(
      "https://aurora.test/",
    );
  });

  it("leaves this app's own root routes alone", () => {
    expect(proxy(makeRequest("/login")).headers.get("location")).toBeNull();
    expect(
      proxy(makeRequest("/auth/callback?code=abc")).headers.get("location"),
    ).toBeNull();
  });

  it("treats the same segment as an ordinary slug below the root", () => {
    // A reserved word is only reserved in the first position: /acme/skills is a
    // workspace route, /skills is not.
    expect(proxy(makeRequest("/acme/skills")).headers.get("location")).toBeNull();
    expect(proxy(makeRequest("/acme/billing")).headers.get("location")).toBeNull();
  });

  it("leaves backend paths to the rewrites", () => {
    // `api`, `v1`, `ws`, `health` and `uploads` are reserved slugs as well, but
    // they are the backend's surface rather than a global Multica route, so
    // neither branch of this proxy may claim them: with no runtime origin
    // configured they fall through to next.config.ts's dev rewrites.
    withoutRuntimeUpstream(() => {
      for (const path of [
        "/api/aurora/skills",
        "/v1/models",
        "/uploads/asset.png",
        "/ws",
        "/health",
      ]) {
        const response = proxy(makeRequest(path));
        expect(response.headers.get("location")).toBeNull();
        expect(response.headers.get("x-middleware-rewrite")).toBeNull();
      }
    });
  });
});

describe("proxy locale header", () => {
  it("forwards the cookie's locale to the rendered request", () => {
    const response = proxy(makeRequest("/login", { [LOCALE_COOKIE]: "ja" }));
    expect(response.headers.get(`x-middleware-request-${MULTICA_LOCALE_HEADER}`)).toBe(
      "ja",
    );
  });

  it("falls back to the Accept-Language header", () => {
    const response = proxy(
      makeRequest("/login", {}, { "accept-language": "ko-KR,ko;q=0.9" }),
    );
    expect(response.headers.get(`x-middleware-request-${MULTICA_LOCALE_HEADER}`)).toBe(
      "ko",
    );
  });

  it("uses the default locale when nothing negotiates", () => {
    const response = proxy(makeRequest("/login"));
    expect(response.headers.get(`x-middleware-request-${MULTICA_LOCALE_HEADER}`)).toBe(
      "en",
    );
  });
});

describe("proxy matcher", () => {
  it("covers the runtime proxy routes", () => {
    expect(config.matcher).toContain("/api/:path*");
    expect(config.matcher).toContain("/ws");
  });
});
