import { afterEach, describe, expect, it, vi } from "vitest";
import type { Workspace } from "../types";
import { paths } from "./paths";
import {
  resolvePostAuthDestination,
  resolveWorkspaceDestination,
  setWorkspaceDestinationResolver,
} from "./resolve";

function makeWs(slug: string): Workspace {
  return {
    id: `id-${slug}`,
    name: slug,
    slug,
    description: null,
    context: null,
    settings: {},
    repos: [],
    issue_prefix: slug.toUpperCase(),
    avatar_url: null,
    created_at: "",
    updated_at: "",
  };
}

describe("resolvePostAuthDestination", () => {
  it("!onboarded → /onboarding (even with a workspace)", () => {
    // V3 invariant: onboarded_at is the single source of truth for
    // workspace access. A user holding workspaces but flagged !onboarded
    // (rare mid-flow state: closed app between Step 2 and Step 3) gets
    // routed to /onboarding so they can finish; the layout hard gate
    // would redirect them anyway.
    const ws = [makeWs("acme")];
    expect(resolvePostAuthDestination(ws, false)).toBe(paths.onboarding());
    expect(resolvePostAuthDestination([], false)).toBe(paths.onboarding());
  });

  it("onboarded + workspace[0] → /<first.slug>/issues", () => {
    const ws = [makeWs("acme"), makeWs("beta")];
    expect(resolvePostAuthDestination(ws, true)).toBe(
      paths.workspace("acme").issues(),
    );
  });

  it("onboarded + no workspace → /workspaces/new", () => {
    // Already-onboarded user without any workspace — usually a returning
    // user whose last workspace got deleted or who left it. They skip
    // re-onboarding and go straight to workspace creation.
    expect(resolvePostAuthDestination([], true)).toBe(paths.newWorkspace());
  });
});

describe("resolveWorkspaceDestination", () => {
  afterEach(() => {
    setWorkspaceDestinationResolver(null);
  });

  it("answers exactly as resolvePostAuthDestination does until an app injects", () => {
    // The shared call sites used to call resolvePostAuthDestination directly.
    // Web and desktop inject nothing, so every branch has to come out
    // byte-for-byte the same or this change moves their users.
    const ws = [makeWs("acme")];
    expect(
      resolveWorkspaceDestination({ workspaces: ws, hasOnboarded: false }),
    ).toBe(resolvePostAuthDestination(ws, false));
    expect(
      resolveWorkspaceDestination({ workspaces: ws, hasOnboarded: true }),
    ).toBe(resolvePostAuthDestination(ws, true));
    expect(
      resolveWorkspaceDestination({ workspaces: [], hasOnboarded: true }),
    ).toBe(resolvePostAuthDestination([], true));
  });

  it("returns the injected app's destination and passes it the context", () => {
    const injected = vi.fn(() => "/aurora/skills");
    setWorkspaceDestinationResolver(injected);
    const ws = [makeWs("acme")];

    expect(
      resolveWorkspaceDestination({ workspaces: ws, hasOnboarded: false }),
    ).toBe("/aurora/skills");
    expect(injected).toHaveBeenCalledWith({
      workspaces: ws,
      hasOnboarded: false,
    });
  });

  it("goes back to the default when the injection is cleared", () => {
    setWorkspaceDestinationResolver(() => "/elsewhere");
    setWorkspaceDestinationResolver(null);

    expect(
      resolveWorkspaceDestination({ workspaces: [], hasOnboarded: true }),
    ).toBe(paths.newWorkspace());
  });
});
