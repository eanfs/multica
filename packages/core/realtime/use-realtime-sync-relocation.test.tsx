/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { WSClient } from "../api/ws-client";
import { useRealtimeSync, type RealtimeSyncStores } from "./use-realtime-sync";

// The destination after workspace loss belongs to the app (#56), so the handler
// has to ask the injectable resolver instead of a hard-coded Multica route.
// This mock stands in for whatever an app injected. It answers with a fragment
// so jsdom can carry the full-page navigation out; a path would hit jsdom's
// unimplemented cross-document navigation.
const resolveWorkspaceDestination = vi.hoisted(() =>
  vi.fn(() => "#relocated"),
);

vi.mock("../paths", () => ({
  useHasOnboarded: () => true,
  resolveWorkspaceDestination,
}));

vi.mock("../api", () => ({
  api: {
    listWorkspaces: async () => [{ id: "ws-2", slug: "still-mine" }],
  },
}));

vi.mock("../platform/workspace-storage", () => ({
  getCurrentWsId: () => "ws-1",
  getCurrentSlug: () => "test-ws",
  // Draft stores are loaded transitively (storage-cleanup → register-all-drafts)
  // so their persist wiring must resolve against this mock.
  createWorkspaceAwareStorage: (adapter: unknown) => adapter,
  registerForWorkspaceRehydration: () => {},
}));

function createRecordingWs(): {
  ws: WSClient;
  handlers: Record<string, (p: unknown) => void>;
} {
  const handlers: Record<string, (p: unknown) => void> = {};
  const ws = {
    on: vi.fn((event: string, handler: (p: unknown) => void) => {
      handlers[event] = handler;
      return () => {};
    }),
    onAny: vi.fn(() => () => {}),
    onReconnect: vi.fn(() => () => {}),
  } as unknown as WSClient;
  return { ws, handlers };
}

function createStores(): RealtimeSyncStores {
  return {
    authStore: Object.assign(() => ({}), {
      getState: () => ({ user: { id: "u1" } }),
      subscribe: () => () => {},
      setState: () => {},
      destroy: () => {},
    }),
  } as unknown as RealtimeSyncStores;
}

function createWrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("useRealtimeSync — relocation after workspace loss", () => {
  afterEach(() => {
    resolveWorkspaceDestination.mockClear();
    window.location.hash = "";
  });

  it("resolves the destination through the app-injected resolver", async () => {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const { ws, handlers } = createRecordingWs();
    renderHook(() => useRealtimeSync(ws, createStores()), {
      wrapper: createWrapper(qc),
    });

    // "ws-1" is the mocked current workspace, so this is a delete another
    // client performed — the branch that relocates.
    const onWorkspaceDeleted = handlers["workspace:deleted"];
    expect(onWorkspaceDeleted).toBeDefined();
    onWorkspaceDeleted!({ workspace_id: "ws-1" });

    await waitFor(() =>
      expect(resolveWorkspaceDestination).toHaveBeenCalledWith({
        workspaces: [{ id: "ws-2", slug: "still-mine" }],
        hasOnboarded: true,
      }),
    );
    await waitFor(() => expect(window.location.hash).toBe("#relocated"));
  });
});
