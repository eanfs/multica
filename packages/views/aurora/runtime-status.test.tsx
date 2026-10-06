import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import { renderWithI18n } from "../test/i18n";

// The view resolves its own workspace and drives the real query/parser stack,
// so the test replaces the API client at @multica/core/api — the seam the
// production client is installed through — rather than mocking the hooks. No
// next/* or react-router-dom module is mocked: the component does not reach a
// router at all, which is part of what this suite pins.
vi.mock("@multica/core/hooks", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/hooks")>()),
  useWorkspaceId: () => "ws-1",
}));

import { RuntimeStatus } from "./runtime-status";

type RuntimeBody = unknown;

type RouteHandler = (path: string) => unknown;

/** Installs a read-only API client that routes by path. */
function installApi(handler: RouteHandler) {
  setApiInstance({
    requestJson: async (path: string) => handler(path),
  } as unknown as ApiClient);
}

const TARGET: RuntimeBody = {
  workspaceId: "ws-1",
  node: {
    id: "node-1",
    status: "online",
    ready: true,
    provider: "docker",
    createdAt: "2026-10-06T00:00:00Z",
  },
  runtimeId: "rt-1",
  state: "online",
};

function generation(overrides: Record<string, unknown> = {}) {
  return {
    id: "gen-1",
    skillId: "poster",
    prompt: "a launch poster",
    status: "running",
    creditsReserved: 760,
    ...overrides,
  };
}

/**
 * Routes the two endpoints the view reads. `runtimeBody` may be a non-object,
 * which is how the malformed-response case is exercised.
 */
function installReads(
  runtimeBody: RuntimeBody,
  generations: unknown[] = [],
) {
  installApi((path) => {
    if (path === "/api/aurora/runtime") return runtimeBody;
    if (path === "/api/aurora/generations") return { generations };
    if (path === "/api/aurora/generations/gen-1") {
      return { generation: generation({ assets: [] }) };
    }
    throw new Error(`unexpected path ${path}`);
  });
}

function renderRuntime() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <RuntimeStatus />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  installReads(TARGET);
});

afterEach(() => {
  cleanup();
  setApiInstance(null as unknown as ApiClient);
});

describe("RuntimeStatus", () => {
  it("shows the online node without a recovery hint", async () => {
    renderRuntime();

    expect(await screen.findByText("Online")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("shows a provisioning node", async () => {
    installReads({ ...(TARGET as object), state: "provisioning" });
    renderRuntime();

    expect(await screen.findByText("Preparing")).toBeInTheDocument();
  });

  it("gives failed nodes recovery guidance", async () => {
    installReads({ ...(TARGET as object), state: "failed" });
    renderRuntime();

    expect(await screen.findByText("Failed")).toBeInTheDocument();
    expect(
      screen.getByText(/execution node failed to start/i),
    ).toBeInTheDocument();
  });

  it("degrades a malformed target to unconfigured with guidance", async () => {
    // GH #55: a malformed 200 must not crash the screen, and "not set up" must
    // read as guidance rather than as a fact the body never carried.
    installReads({ node: "not-an-object" });
    renderRuntime();

    expect(await screen.findByText("Not set up")).toBeInTheDocument();
    expect(
      screen.getByText(/no running execution node/i),
    ).toBeInTheDocument();
  });

  it("normalizes an unknown server state to unconfigured", async () => {
    installReads({ ...(TARGET as object), state: "paused" });
    renderRuntime();

    expect(await screen.findByText("Not set up")).toBeInTheDocument();
  });

  it("does not crash when the node omits its provider", async () => {
    installReads({
      workspaceId: "ws-1",
      node: {
        id: "node-1",
        status: "starting",
        ready: false,
        createdAt: "2026-10-06T00:00:00Z",
      },
      runtimeId: "rt-1",
      state: "provisioning",
    });
    renderRuntime();

    expect(await screen.findByText("Preparing")).toBeInTheDocument();
  });

  it("shows an in-flight generation with its live status", async () => {
    // The list's stored status is still "queued"; the row labels itself from
    // the polled detail read, which is what makes the screen live.
    installReads(TARGET, [generation({ status: "queued" })]);
    renderRuntime();

    expect(await screen.findByText("a launch poster")).toBeInTheDocument();
    expect(await screen.findByText("Generating")).toBeInTheDocument();
    expect(screen.queryByText("Queued")).not.toBeInTheDocument();
  });

  it("offers the empty copy when nothing is running", async () => {
    renderRuntime();

    expect(
      await screen.findByText("Nothing is running."),
    ).toBeInTheDocument();
  });
});
