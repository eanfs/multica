import { vi } from "vitest";
import type { NavigationAdapter } from "../navigation";

/**
 * A `NavigationAdapter` for tests that render shared views outside a real app.
 *
 * Views reach the router only through the adapter, so a test can supply one
 * without mocking `next/*` or `react-router-dom` — which the views test rules
 * forbid, and which would stop testing the adapter indirection that is the
 * whole point of the boundary. `getShareableUrl` echoes its path rather than
 * inventing an origin, so an assertion on a desktop-style link can still read
 * the in-app path back.
 *
 * Spread `overrides` to make one method observable (`push: vi.fn()`) without
 * rebuilding the rest.
 */
export function stubNavigationAdapter(
  overrides: Partial<NavigationAdapter> = {},
): NavigationAdapter {
  return {
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    pathname: "/",
    searchParams: new URLSearchParams(),
    hash: "",
    getShareableUrl: (path) => path,
    ...overrides,
  };
}
