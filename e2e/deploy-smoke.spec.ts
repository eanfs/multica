import {
  test,
  expect,
  type APIRequestContext,
  type Page,
  type TestInfo,
} from "@playwright/test";
import { randomUUID } from "node:crypto";
import { connect as tlsConnect } from "node:tls";

// Read-only deployment verification for the freshly deployed hosts:
//
//   https://aod.apexxai.net   -> multica frontend (after cutover from OpenDesign)
//   https://mcapi.apexxai.net -> multica backend (daemon/CLI + browser /ws)
//   https://od.apexxai.net    -> OpenDesign (after its migration)
//
// The suite creates no accounts, submits no forms, and sends no mutating
// writes. The largest payload is an oversized body to the public logout route,
// which is discarded by the handler; it exists only to prove the WAF body
// inspection limit (SizeRestrictions_BODY) cannot 403 multica.
//
// All checks are tagged @smoke (inherited from the describe block below) and
// run through the dedicated "smoke" project in playwright.deploy.config.ts.

const APP_URL = stripTrailingSlash(
  process.env.MCA_APP_URL ??
    process.env.PLAYWRIGHT_BASE_URL ??
    "https://aod.apexxai.net",
);
const API_URL = stripTrailingSlash(
  process.env.MCA_API_URL ?? "https://mcapi.apexxai.net",
);
const OD_URL = stripTrailingSlash(
  process.env.OD_APP_URL ?? "https://od.apexxai.net",
);
// mca is the Multica web app host; aod now serves the Aurora app (owner
// correction 2026-09-30: aod is for Aurora, not the web app).
const WEB_URL = stripTrailingSlash(
  process.env.MCA_WEB_URL ?? "https://mca.apexxai.net",
);
const WAF_PROBE_PATH = process.env.MCA_WAF_PROBE_PATH ?? "/auth/logout";
const WAF_PROBE_SIZES_KIB = [9, 20];

const MULTICA_TARGETS = [
  { name: "aod (multica frontend)", url: APP_URL },
  { name: "mcapi (multica backend)", url: API_URL },
];

const TLS_TARGETS = [
  { name: "aod.apexxai.net (multica frontend)", url: APP_URL },
  { name: "mcapi.apexxai.net (multica backend)", url: API_URL },
  { name: "od.apexxai.net (OpenDesign)", url: OD_URL },
];

function stripTrailingSlash(value: string): string {
  return value.replace(/\/+$/, "");
}

function hostOf(url: string): string {
  return new URL(url).hostname;
}

function wssUrlFor(baseUrl: string, path: string): string {
  const parsed = new URL(path, baseUrl);
  parsed.protocol = "wss:";
  return parsed.toString();
}

// ---------------------------------------------------------------------------
// Console hygiene: collect errors continuously, fail on the unexpected ones.
// ---------------------------------------------------------------------------

type ConsoleIssue = {
  kind: "console" | "pageerror";
  text: string;
  location?: string;
};

// Errors that production pages legitimately emit for an anonymous visitor.
// Keep this list narrow; every other console error or page error fails a test.
const ALLOWED_CONSOLE_ERROR_PATTERNS: RegExp[] = [
  /favicon\.ico/i,
  /ResizeObserver loop/i,
  /Failed to load resource: the server responded with a status of 401/i,
  // Aurora's API client logs its own formatted 401 for /api/me before login,
  // instead of the browser's generic "Failed to load resource" line. Same
  // expected anonymous-auth signal for a signed-out visitor; keep it narrow.
  /\[api\].*401 \/api\/me/i,
];

function trackConsole(page: Page): ConsoleIssue[] {
  const issues: ConsoleIssue[] = [];
  page.on("console", (message) => {
    if (message.type() !== "error") {
      return;
    }
    const location = message.location();
    const url =
      location && location.url
        ? location.url + ":" + location.lineNumber
        : undefined;
    issues.push({ kind: "console", text: message.text(), location: url });
  });
  page.on("pageerror", (error) => {
    issues.push({ kind: "pageerror", text: error.message });
  });
  return issues;
}

function unexpectedConsoleIssues(issues: ConsoleIssue[]): ConsoleIssue[] {
  return issues.filter((issue) => {
    const haystack = issue.text + " " + (issue.location ?? "");
    return !ALLOWED_CONSOLE_ERROR_PATTERNS.some((pattern) =>
      pattern.test(haystack),
    );
  });
}

async function assertConsoleHygiene(
  issues: ConsoleIssue[],
  testInfo: TestInfo,
): Promise<void> {
  await testInfo.attach("console-issues", {
    body: JSON.stringify(issues, null, 2),
    contentType: "application/json",
  });
  const unexpected = unexpectedConsoleIssues(issues);
  expect(
    unexpected,
    "Unexpected console/page errors:\n" +
      unexpected
        .map(
          (issue) =>
            "[" +
            issue.kind +
            "] " +
            issue.text +
            (issue.location ? " @ " + issue.location : ""),
        )
        .join("\n"),
  ).toEqual([]);
}

// ---------------------------------------------------------------------------
// TLS: validate the presented certificate explicitly. rejectUnauthorized is
// true, so an invalid, expired, or untrusted chain fails the handshake.
// ---------------------------------------------------------------------------

type TlsInfo = {
  authorized: boolean;
  authorizationError?: string;
  protocol?: string;
  subjectCN?: string;
  issuerO?: string;
  validTo?: string;
};

function probeTls(host: string): Promise<TlsInfo> {
  return new Promise<TlsInfo>((resolve, reject) => {
    const socket = tlsConnect(
      {
        host,
        port: 443,
        servername: host,
        rejectUnauthorized: true,
        timeout: 15000,
      },
      () => {
        const certificate = socket.getPeerCertificate();
        resolve({
          authorized: socket.authorized,
          authorizationError: socket.authorizationError
            ? String(socket.authorizationError)
            : undefined,
          protocol: socket.getProtocol() ?? undefined,
          subjectCN:
            certificate && certificate.subject
              ? certificate.subject.CN
              : undefined,
          issuerO:
            certificate && certificate.issuer
              ? certificate.issuer.O
              : undefined,
          validTo: certificate ? certificate.valid_to : undefined,
        });
        socket.end();
      },
    );
    socket.on("error", (error) => {
      socket.destroy();
      reject(error);
    });
    socket.setTimeout(15000, () => {
      socket.destroy(new Error("TLS handshake timed out after 15000ms"));
    });
  });
}

// ---------------------------------------------------------------------------
// WebSocket: open a real browser WebSocket and require the upgrade to open.
// A browser cannot tunnel a WS upgrade through the Next.js HTTP rewrite, so
// this is the topology risk the ALB path rules must cover.
// ---------------------------------------------------------------------------

async function assertWebSocketUpgrade(page: Page, wsUrl: string): Promise<void> {
  const result = await page.evaluate((url: string) => {
    return new Promise<{ ok: boolean; detail: string }>((resolve) => {
      let settled = false;
      let socket: WebSocket | undefined;
      const settle = (ok: boolean, detail: string) => {
        if (settled) {
          return;
        }
        settled = true;
        try {
          socket?.close();
        } catch {
          // already closed
        }
        resolve({ ok, detail });
      };
      try {
        socket = new WebSocket(url);
      } catch (error) {
        resolve({ ok: false, detail: "constructor threw: " + String(error) });
        return;
      }
      socket.onopen = () =>
        settle(
          true,
          "opened (readyState=" + (socket ? socket.readyState : -1) + ")",
        );
      socket.onerror = () => settle(false, "error before open");
      socket.onclose = (event) =>
        settle(
          false,
          "closed before open (code=" + event.code + ", reason=" + event.reason + ")",
        );
      setTimeout(() => settle(false, "timed out waiting for open"), 15000);
    });
  }, wsUrl);
  expect(result.ok, "WebSocket " + wsUrl + ": " + result.detail).toBe(true);
}

// ---------------------------------------------------------------------------
// WAF body limit: a non-mutating oversized POST must reach the app. The AWS
// WAF SizeRestrictions_BODY rule blocks at 8 KiB unless the host/method
// exemption is in place; this exact class of 403 previously broke OpenDesign.
// ---------------------------------------------------------------------------

async function assertLargePostReachesApp(
  request: APIRequestContext,
  baseUrl: string,
  kib: number,
): Promise<void> {
  const payload = '{"probe":"' + "a".repeat(kib * 1024) + '"}';
  const target = baseUrl + WAF_PROBE_PATH;
  const response = await request.post(target, {
    data: payload,
    headers: { "content-type": "application/json" },
    timeout: 20000,
  });
  const status = response.status();
  const body = status >= 400 ? (await response.text()).slice(0, 400) : "";
  const bytes = Buffer.byteLength(payload);
  expect(new URL(response.url()).protocol, "request must use https").toBe(
    "https:",
  );
  expect(
    status,
    "POST " +
      target +
      " (" +
      bytes +
      " bytes) returned HTTP " +
      status +
      "; headers=" +
      JSON.stringify(response.headers()) +
      "; body=" +
      body,
  ).not.toBe(403);
  expect(
    status,
    "POST " + target + " (" + bytes + " bytes) should reach the app, got HTTP " + status,
  ).toBeLessThan(500);
}

test.describe("Deployed host smoke", { tag: "@smoke" }, () => {
  for (const target of MULTICA_TARGETS) {
    test(
      "GET /api/config returns 200 with the expected JSON shape on " +
        target.name,
      async ({ request }) => {
        const response = await request.get(target.url + "/api/config", {
          timeout: 20000,
        });
        expect(new URL(response.url()).protocol).toBe("https:");
        expect(response.status()).toBe(200);
        expect(response.headers()["content-type"] ?? "").toContain(
          "application/json",
        );
        const body = await response.json();
        expect(body).toMatchObject({
          allow_signup: expect.any(Boolean),
          posthog_key: expect.any(String),
          posthog_host: expect.any(String),
          analytics_environment: expect.any(String),
          local_worktree_supported: expect.any(Boolean),
          agent_conversation_starters_supported: expect.any(Boolean),
          comment_delete_keep_replies_supported: expect.any(Boolean),
        });
        expect(body.feature_flags).toBeInstanceOf(Object);
      },
    );

    for (const kib of WAF_PROBE_SIZES_KIB) {
      test(
        "WAF body limit: a " +
          kib +
          " KiB POST is not blocked with 403 on " +
          target.name,
        async ({ request }) => {
          await assertLargePostReachesApp(request, target.url, kib);
        },
      );
    }
  }

  test(
    "GET /readyz returns 200 with the expected readiness shape on mcapi",
    async ({ request }) => {
      const response = await request.get(API_URL + "/readyz", {
        timeout: 20000,
      });
      expect(new URL(response.url()).protocol).toBe("https:");
      expect(response.status()).toBe(200);
      expect(response.headers()["content-type"] ?? "").toContain(
        "application/json",
      );
      const body = await response.json();
      expect(body).toMatchObject({
        status: "ok",
        checks: { db: "ok", migrations: "ok" },
      });
    },
  );

  test("aod renders the Aurora app shell", async ({ page }, testInfo) => {
    const consoleIssues = trackConsole(page);
    const response = await page.goto(APP_URL + "/", {
      waitUntil: "domcontentloaded",
    });
    expect(response, "no navigation response for " + APP_URL + "/").not.toBeNull();
    expect(
      response!.status(),
      "GET " + APP_URL + "/ returned HTTP " + response!.status(),
    ).toBe(200);
    expect(new URL(page.url()).protocol).toBe("https:");
    // Aurora-specific marker rather than a generic shell: apps/aurora/app/layout.tsx
    // sets metadata.title = "Aurora", while apps/web sets "Multica — ...".
    await expect(page).toHaveTitle(/Aurora/);
    // The root server-redirects the anonymous visitor to /login; the shared
    // login form's email field is the user-facing shell signal.
    await expect(
      page.getByRole("textbox", { name: /email/i }).first(),
      "Aurora login email field is not visible",
    ).toBeVisible();
    await assertConsoleHygiene(consoleIssues, testInfo);
  });

  test("mca renders the multica web app shell", async ({ page }, testInfo) => {
    const consoleIssues = trackConsole(page);
    const response = await page.goto(WEB_URL + "/", {
      waitUntil: "domcontentloaded",
    });
    expect(response, "no navigation response for " + WEB_URL + "/").not.toBeNull();
    expect(
      response!.status(),
      "GET " + WEB_URL + "/ returned HTTP " + response!.status(),
    ).toBe(200);
    expect(new URL(page.url()).protocol).toBe("https:");
    await expect(page).toHaveTitle(/Multica/);
    const shell = page
      .getByRole("heading", { level: 1 })
      .or(page.getByRole("textbox", { name: /email/i }))
      .first();
    await expect(
      shell,
      "multica web app shell (level-1 heading or login email field) is not visible",
    ).toBeVisible();
    await assertConsoleHygiene(consoleIssues, testInfo);
  });

  test(
    "od renders the OpenDesign app shell (migration target)",
    async ({ page }, testInfo) => {
      const consoleIssues = trackConsole(page);
      const response = await page.goto(OD_URL + "/", {
        waitUntil: "domcontentloaded",
      });
      expect(response, "no navigation response for " + OD_URL + "/").not.toBeNull();
      expect(
        response!.status(),
        "GET " + OD_URL + "/ returned HTTP " + response!.status(),
      ).toBe(200);
      expect(new URL(page.url()).protocol).toBe("https:");
      const shell = page
        .getByRole("heading")
        .or(page.getByRole("textbox"))
        .or(page.getByTestId("home-hero"))
        .first();
      await expect(
        shell,
        "OpenDesign app shell (heading, textbox, or home hero) is not visible",
      ).toBeVisible();
      await assertConsoleHygiene(consoleIssues, testInfo);
    },
  );

  test(
    "browser WebSocket upgrades to /ws on aod (same-origin via ALB)",
    async ({ page }) => {
      await page.goto(APP_URL + "/login", { waitUntil: "domcontentloaded" });
      const wsUrl =
        wssUrlFor(APP_URL, "/ws") +
        "?workspace_id=" +
        randomUUID() +
        "&client_platform=web&client_version=deploy-smoke";
      await assertWebSocketUpgrade(page, wsUrl);
    },
  );

  test(
    "browser WebSocket upgrades to /ws on mcapi (backend direct)",
    async ({ page }) => {
      await page.goto(API_URL + "/readyz", { waitUntil: "domcontentloaded" });
      const wsUrl =
        wssUrlFor(API_URL, "/ws") +
        "?workspace_id=" +
        randomUUID() +
        "&client_platform=web&client_version=deploy-smoke";
      await assertWebSocketUpgrade(page, wsUrl);
    },
  );

  for (const target of TLS_TARGETS) {
    test(
      "TLS certificate for " +
        target.name +
        " is valid without disabling verification",
      async () => {
        const info = await probeTls(hostOf(target.url));
        expect(
          info.authorized,
          "certificate not trusted" +
            (info.authorizationError
              ? ": " + info.authorizationError
              : " (rejectUnauthorized=true)"),
        ).toBe(true);
        expect(info.authorizationError).toBeUndefined();
        expect(info.protocol ?? "").toMatch(/^TLSv1\.[23]$/);
        const expiresAt = info.validTo ? Date.parse(info.validTo) : Number.NaN;
        expect(
          Number.isNaN(expiresAt),
          "unparsable certificate valid_to: " + String(info.validTo),
        ).toBe(false);
        expect(expiresAt).toBeGreaterThan(Date.now());
      },
    );
  }
});
