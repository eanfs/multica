import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import type { NavigationAdapter } from "@multica/views/navigation";
import { NavigationProvider } from "@multica/views/navigation";
import { renderWithI18n } from "../test/render";

// Logging out tears down the whole client session, which needs the CoreProvider
// the shell is normally mounted under. The shell's job here is to offer the
// action, not to perform it.
vi.mock("@multica/views/auth", () => ({ useLogout: () => vi.fn() }));

import { AuroraShell } from "./aurora-shell";

function stubAdapter(pathname: string): NavigationAdapter {
  return {
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    pathname,
    searchParams: new URLSearchParams(),
    hash: "",
    getShareableUrl: (path) => path,
  };
}

function renderShell(pathname: string) {
  return renderWithI18n(
    <NavigationProvider value={stubAdapter(pathname)}>
      <AuroraShell slug="acme">
        <p>page</p>
      </AuroraShell>
    </NavigationProvider>,
  );
}

describe("AuroraShell", () => {
  it("links every destination the app serves", () => {
    renderShell("/acme/skills");

    expect(screen.getByRole("navigation", { name: "Aurora" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Create" })).toHaveAttribute(
      "href",
      "/acme/skills",
    );
    expect(screen.getByRole("link", { name: "My works" })).toHaveAttribute(
      "href",
      "/acme/works",
    );
    expect(screen.getByRole("link", { name: "Credits" })).toHaveAttribute(
      "href",
      "/acme/billing",
    );
  });

  it("marks exactly the destination being viewed", () => {
    renderShell("/acme/works");

    expect(screen.getByRole("link", { name: "My works" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(screen.getByRole("link", { name: "Create" })).not.toHaveAttribute(
      "aria-current",
    );
    expect(screen.getByRole("link", { name: "Credits" })).not.toHaveAttribute(
      "aria-current",
    );
  });

  it("counts a trailing slash as the same destination", () => {
    renderShell("/acme/billing/");

    expect(screen.getByRole("link", { name: "Credits" })).toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  it("marks nothing on a route the nav does not own", () => {
    renderShell("/acme/works/abc");

    for (const name of ["Create", "My works", "Credits"]) {
      expect(screen.getByRole("link", { name })).not.toHaveAttribute(
        "aria-current",
      );
    }
  });

  it("renders the page it wraps", () => {
    renderShell("/acme/skills");
    expect(screen.getByText("page")).toBeVisible();
  });

  it("offers the account action", () => {
    renderShell("/acme/skills");
    expect(screen.getByRole("button", { name: "Log out" })).toBeVisible();
  });
});
