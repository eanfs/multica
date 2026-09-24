import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import type { SupportedLocale } from "@multica/core/i18n";
import { I18nProvider } from "@multica/core/i18n/react";
import { RESOURCES } from "@multica/views/locales";
import {
  OAUTH_PENDING_NONCES_KEY,
  beginGoogleOAuthFlow,
  generateOAuthNonce,
} from "@multica/views/auth";

const {
  mockPush,
  mockRouter,
  mockSearchParams,
  mockLoginWithGoogle,
  mockEnsureQueryData,
  mockQueryClient,
} = vi.hoisted(() => {
  const mockPush = vi.fn();
  const mockEnsureQueryData = vi.fn();
  return {
    mockPush,
    // The page's effect depends on the router and the query client. Both must
    // be stable objects, the way the real hooks are: a fresh object per render
    // would re-run the exchange on every render.
    mockRouter: { push: mockPush },
    mockSearchParams: new URLSearchParams(),
    mockLoginWithGoogle: vi.fn(),
    mockEnsureQueryData,
    mockQueryClient: { ensureQueryData: mockEnsureQueryData },
  };
});

vi.mock("next/navigation", () => ({
  useRouter: () => mockRouter,
  useSearchParams: () => mockSearchParams,
}));

vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => mockQueryClient,
}));

vi.mock("@multica/core/logger", async () => {
  const actual =
    await vi.importActual<typeof import("@multica/core/logger")>(
      "@multica/core/logger",
    );
  return {
    ...actual,
    createLogger: () => ({
      debug: vi.fn(),
      info: vi.fn(),
      warn: vi.fn(),
      error: vi.fn(),
    }),
  };
});

// Keep the real sanitizeNextUrl so the redirect rules are exercised rather
// than silently diverging behind a mock reimplementation.
vi.mock("@multica/core/auth", async () => {
  const actual =
    await vi.importActual<typeof import("@multica/core/auth")>(
      "@multica/core/auth",
    );
  return {
    ...actual,
    useAuthStore: (selector: (s: unknown) => unknown) =>
      selector({ loginWithGoogle: mockLoginWithGoogle }),
  };
});

vi.mock("@multica/core/workspace/queries", () => ({
  workspaceListOptions: () => ({ queryKey: ["workspaces", "list"] }),
}));

// The notice reaches for the navigation provider, which is the shell's job to
// supply — not something this page's behavior depends on.
vi.mock("@/components/no-workspace-notice", () => ({
  NoWorkspaceNotice: () => <div>no workspace</div>,
}));

import CallbackPage from "./page";

/**
 * Build the `state` the login page would have handed to Google: carriers
 * joined behind a nonce this browser minted.
 */
function oauthState(...carriers: string[]): string {
  return beginGoogleOAuthFlow(carriers);
}

function renderCallback(locale: SupportedLocale = "zh-Hans") {
  return render(
    <I18nProvider locale={locale} resources={RESOURCES}>
      <CallbackPage />
    </I18nProvider>,
  );
}

const CSRF_REJECTION = "本次登录并非从当前浏览器发起，请重新登录。";

describe("Aurora CallbackPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    window.localStorage.removeItem(OAUTH_PENDING_NONCES_KEY);
    Array.from(mockSearchParams.keys()).forEach((k) =>
      mockSearchParams.delete(k),
    );
    mockSearchParams.set("code", "test-code");
    mockLoginWithGoogle.mockResolvedValue({ id: "user-1" });
    mockEnsureQueryData.mockResolvedValue([]);
  });

  // Regression: #53 — the exchange used to run for any code in the URL, so a
  // crafted callback could bind this browser to the attacker's account.
  describe("CSRF gate (#53)", () => {
    it.each([
      ["no state at all", null],
      ["a state with no nonce field", "next:/acme/works"],
      ["a well-formed nonce this browser never minted", null],
    ])("refuses to exchange the code given %s", async (label, state) => {
      if (label === "no state at all") mockSearchParams.delete("state");
      else if (state === null) {
        mockSearchParams.set("state", `nonce:${generateOAuthNonce()},next:/x`);
      } else mockSearchParams.set("state", state);

      renderCallback();

      expect(await screen.findByText(CSRF_REJECTION)).toBeInTheDocument();
      expect(mockLoginWithGoogle).not.toHaveBeenCalled();
      expect(mockPush).not.toHaveBeenCalled();
    });

    it("exchanges the code once when the nonce matches", async () => {
      mockSearchParams.set("state", oauthState("next:/acme/works"));

      renderCallback();

      await waitFor(() => {
        expect(mockLoginWithGoogle).toHaveBeenCalledWith(
          "test-code",
          expect.stringContaining("/auth/callback"),
        );
      });
      await waitFor(() => {
        expect(mockPush).toHaveBeenCalledWith("/acme/works");
      });
    });

    it("refuses a captured callback URL replayed a second time", async () => {
      mockSearchParams.set("state", oauthState());

      const first = renderCallback();
      await waitFor(() => {
        expect(mockLoginWithGoogle).toHaveBeenCalledTimes(1);
      });
      first.unmount();

      renderCallback();

      expect(await screen.findByText(CSRF_REJECTION)).toBeInTheDocument();
      expect(mockLoginWithGoogle).toHaveBeenCalledTimes(1);
    });
  });

  describe("post-login destination", () => {
    it("falls back to the resolved workspace when no next= was carried", async () => {
      mockSearchParams.set("state", oauthState());
      mockEnsureQueryData.mockResolvedValue([{ slug: "acme" }]);

      renderCallback();

      await waitFor(() => {
        expect(mockPush).toHaveBeenCalledWith("/acme/skills");
      });
    });

    it("ignores an unsafe next= target", async () => {
      mockSearchParams.set("state", oauthState("next:https://evil.example"));
      mockEnsureQueryData.mockResolvedValue([{ slug: "acme" }]);

      renderCallback();

      await waitFor(() => {
        expect(mockPush).toHaveBeenCalledWith("/acme/skills");
      });
      expect(mockPush).not.toHaveBeenCalledWith("https://evil.example");
    });
  });
});
