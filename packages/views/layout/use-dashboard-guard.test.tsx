import { renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setWorkspaceDestinationResolver } from "@multica/core/paths";
import { useDashboardGuard } from "./use-dashboard-guard";

const replace = vi.fn();
const mockWorkspaces = vi.hoisted(() => [{ slug: "valid-team" }]);

vi.mock("../navigation", () => ({
  useNavigation: () => ({ pathname: "/stale-slug", replace }),
}));

// Keep the real resolver module so the test can register an app resolver the
// same way a host app does; only the two reads the guard needs are stubbed.
vi.mock("@multica/core/paths", async () => {
  const actual =
    await vi.importActual<typeof import("@multica/core/paths")>(
      "@multica/core/paths",
    );
  return {
    ...actual,
    useCurrentWorkspace: () => null,
    useHasOnboarded: () => true,
  };
});

vi.mock("@multica/core/auth", async () => {
  const actual =
    await vi.importActual<typeof import("@multica/core/auth")>(
      "@multica/core/auth",
    );
  type AuthState = { user: { id: string }; isLoading: boolean };
  const state = (): AuthState => ({ user: { id: "u1" }, isLoading: false });
  const useAuthStore = Object.assign(
    (sel?: (s: AuthState) => unknown) => (sel ? sel(state()) : state()),
    { getState: state },
  );
  return { ...actual, useAuthStore };
});

vi.mock("@multica/core/workspace", () => ({
  useWorkspaceList: () => ({ workspaces: mockWorkspaces, ready: true }),
}));

vi.mock("@multica/core/navigation", () => ({
  useNavigationStore: { getState: () => ({ onPathChange: () => {} }) },
}));

vi.mock("@multica/core/issues/stores", () => ({
  useRecentIssuesStore: { getState: () => ({ pruneWorkspaces: () => {} }) },
}));

describe("useDashboardGuard — unresolved workspace destination", () => {
  beforeEach(() => {
    replace.mockReset();
  });

  afterEach(() => {
    // The destination resolver is module-global; leave the default behind.
    setWorkspaceDestinationResolver(null);
  });

  it("falls back to the shared default when the app injects nothing", () => {
    renderHook(() => useDashboardGuard());
    expect(replace).toHaveBeenCalledWith("/valid-team/issues");
  });

  it("redirects to the destination the host app injected", () => {
    // Shared views used to hard-code resolvePostAuthDestination, which sends a
    // non-Multica client to a route it does not serve (#56).
    setWorkspaceDestinationResolver(() => "/elsewhere/skills");
    renderHook(() => useDashboardGuard());
    expect(replace).toHaveBeenCalledWith("/elsewhere/skills");
  });
});
