import { afterEach, describe, expect, it } from "vitest";
import { renderHook } from "@testing-library/react";
import {
  resolveWorkspaceDestination,
  setWorkspaceDestinationResolver,
} from "@multica/core/paths";
import type { Workspace } from "@multica/core/types";
import {
  resolveAuroraWorkspaceDestination,
  useAuroraWorkspaceDestination,
} from "./workspace-destination";

// Only `slug` is read — the resolver never touches the rest of the record.
const workspace = (slug: string) => ({ slug }) as Workspace;

describe("resolveAuroraWorkspaceDestination", () => {
  it("opens the workspace the user still has, whatever onboarded_at says", () => {
    // onboarded_at gates the Multica questionnaire, not access to this app, so
    // it must not steer the destination.
    expect(
      resolveAuroraWorkspaceDestination({
        workspaces: [workspace("acme")],
        hasOnboarded: false,
      }),
    ).toBe("/acme/skills");
    expect(
      resolveAuroraWorkspaceDestination({
        workspaces: [workspace("acme")],
        hasOnboarded: true,
      }),
    ).toBe("/acme/skills");
  });

  it("sends an account with no workspace to the screen that explains it", () => {
    // Aurora serves no workspace-creation route, so the alternative to
    // core's /workspaces/new is /login, where NoWorkspaceNotice offers the
    // sign-in that opens the personal workspace again.
    expect(
      resolveAuroraWorkspaceDestination({
        workspaces: [],
        hasOnboarded: true,
      }),
    ).toBe("/login");
  });
});

describe("useAuroraWorkspaceDestination", () => {
  afterEach(() => {
    setWorkspaceDestinationResolver(null);
  });

  it("registers the app's destination with shared core", () => {
    const { unmount } = renderHook(() => useAuroraWorkspaceDestination());

    // Read through core's resolver — the one the realtime relocate handler
    // calls — so this asserts the route the app actually lands on.
    expect(
      resolveWorkspaceDestination({
        workspaces: [workspace("acme")],
        hasOnboarded: false,
      }),
    ).toBe("/acme/skills");

    unmount();

    // With the shell gone, core's Multica-web default is back.
    expect(
      resolveWorkspaceDestination({
        workspaces: [workspace("acme")],
        hasOnboarded: true,
      }),
    ).toBe("/acme/issues");
  });
});
