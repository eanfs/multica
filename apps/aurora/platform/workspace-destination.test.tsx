import { afterEach, describe, expect, it } from "vitest";
import { render, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
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
    // core's /workspaces/new is /login, where NoWorkspaceNotice explains the
    // state and offers a log-out button; the next sign-in reopens the
    // personal workspace.
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

  it("registers during render, so a render-time reader sees the app route on the first paint", () => {
    // Regression: shared views (InvitePage) derive an href from the
    // module-global resolver while rendering. React runs child effects before
    // parent effects, so an effect-only registration would leave that child's
    // first render on core's Multica default. Capture the value at render time
    // — no waitFor, no extra effect flush — and assert the child saw Aurora.
    let childSaw: string | null = null;

    function Child() {
      childSaw = resolveWorkspaceDestination({
        workspaces: [workspace("acme")],
        hasOnboarded: true,
      });
      return null;
    }

    function Shell({ children }: { children: ReactNode }) {
      useAuroraWorkspaceDestination();
      return <>{children}</>;
    }

    render(
      <Shell>
        <Child />
      </Shell>,
    );

    expect(childSaw).toBe("/acme/skills");
  });
});
