/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { ApiError, setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { isAuroraInsufficientCreditsError } from "./api";
import { useCreateAuroraGeneration, useDeleteAuroraAsset } from "./mutations";
import { auroraKeys, auroraWalletKeys } from "./queries";
import type { AuroraAsset } from "./schema";

vi.mock("../hooks", () => ({ useWorkspaceId: () => "ws-1" }));

const asset: AuroraAsset = {
  id: "asset-1",
  generationId: "gen-1",
  kind: "image",
  mediaUrl: "https://cdn.example/a.png",
  format: "png",
  createdAt: "2026-09-22T00:00:00Z",
};

function wrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

function newClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
}

/** The query keys an invalidation sweep touched, as comparable strings. */
function invalidatedKeys(invalidate: { mock: { calls: unknown[][] } }): string[] {
  return invalidate.mock.calls.map(([arg]) =>
    JSON.stringify((arg as { queryKey?: unknown })?.queryKey),
  );
}

describe("useCreateAuroraGeneration", () => {
  it("refreshes the wallet and the generations once the create settles", async () => {
    setApiInstance({
      requestJson: vi.fn(async () => ({
        generation: {
          id: "gen-1",
          skillId: "poster",
          prompt: "a cat",
          status: "queued",
          creditsReserved: 760,
        },
      })),
    } as unknown as ApiClient);
    const qc = newClient();
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useCreateAuroraGeneration(), {
      wrapper: wrapper(qc),
    });

    await act(async () => {
      await result.current.mutateAsync({ skillId: "poster", prompt: "a cat" });
    });

    const keys = invalidatedKeys(invalidate);
    // Create reserved credits before it returned, so the balance on screen is
    // stale the moment it settles.
    expect(keys).toContain(JSON.stringify(auroraWalletKeys.all()));
    expect(keys).toContain(JSON.stringify(auroraKeys.generations("ws-1")));
  });

  it("hands a 402 to the caller and still refreshes the wallet", async () => {
    setApiInstance({
      requestJson: vi.fn(async () => {
        throw new ApiError("insufficient credits", 402, "Payment Required");
      }),
    } as unknown as ApiClient);
    const qc = newClient();
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useCreateAuroraGeneration(), {
      wrapper: wrapper(qc),
    });

    await act(async () => {
      await expect(
        result.current.mutateAsync({ skillId: "poster", prompt: "a cat" }),
      ).rejects.toMatchObject({ status: 402 });
    });

    await waitFor(() =>
      expect(isAuroraInsufficientCreditsError(result.current.error)).toBe(true),
    );
    // The wallet the user is looking at was read before another surface spent
    // it, which is exactly how a 402 happens — so it is refreshed on failure
    // too.
    expect(invalidatedKeys(invalidate)).toContain(
      JSON.stringify(auroraWalletKeys.all()),
    );
  });
});

describe("useDeleteAuroraAsset", () => {
  it("leaves the cached library alone until the server confirms", async () => {
    let release: (() => void) | undefined;
    const write = new Promise<void>((resolve) => {
      release = resolve;
    });
    setApiInstance({
      requestJson: vi.fn(() => write),
    } as unknown as ApiClient);
    const qc = newClient();
    qc.setQueryData(auroraKeys.assets("ws-1"), [asset]);
    const { result } = renderHook(() => useDeleteAuroraAsset(), {
      wrapper: wrapper(qc),
    });

    act(() => {
      void result.current.mutate(asset.id);
    });

    // Not optimistic: the server also drops the stored object, so a row that
    // survived a rolled-back delete would offer a file that is already gone.
    expect(qc.getQueryData(auroraKeys.assets("ws-1"))).toEqual([asset]);

    await act(async () => {
      release?.();
      await write;
    });
  });

  it("refreshes the library and the generation list once the delete settles", async () => {
    setApiInstance({
      requestJson: vi.fn(async () => undefined),
    } as unknown as ApiClient);
    const qc = newClient();
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useDeleteAuroraAsset(), {
      wrapper: wrapper(qc),
    });

    await act(async () => {
      await result.current.mutateAsync(asset.id);
    });

    const keys = invalidatedKeys(invalidate);
    expect(keys).toContain(JSON.stringify(auroraKeys.assets("ws-1")));
    // The same asset is listed inside its generation's detail.
    expect(keys).toContain(JSON.stringify(auroraKeys.generations("ws-1")));
  });
});
