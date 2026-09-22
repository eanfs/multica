// @vitest-environment node
import { describe, expect, it } from "vitest";
import { auroraGenerationDetailOptions, auroraKeys, auroraWalletKeys } from "./queries";
import { AURORA_GENERATION_POLL_MS } from "./types";

/**
 * Pulls the polling decision out of the options object so the contract can be
 * pinned without fake timers. A `false` answer is what stops the poll; a number
 * is the next tick.
 */
function pollInterval(status: string | undefined): number | false {
  const options = auroraGenerationDetailOptions("ws-1", "gen-1");
  const refetchInterval = options.refetchInterval as unknown as (query: {
    state: { data: { status: string } | null };
  }) => number | false;
  return refetchInterval({
    state: { data: status === undefined ? null : { status } },
  });
}

describe("auroraGenerationDetailOptions", () => {
  it("polls a generation that is still in flight", () => {
    expect(pollInterval("queued")).toBe(AURORA_GENERATION_POLL_MS);
    expect(pollInterval("running")).toBe(AURORA_GENERATION_POLL_MS);
  });

  it("stops polling once the generation is final", () => {
    expect(pollInterval("completed")).toBe(false);
    expect(pollInterval("failed")).toBe(false);
  });

  it("keeps polling when the detail body could not be read", () => {
    // null is what parseAuroraGenerationDetail falls back to; it is not a
    // finished generation, so the screen keeps trying rather than parking.
    expect(pollInterval(undefined)).toBe(AURORA_GENERATION_POLL_MS);
  });
});

describe("auroraKeys", () => {
  it("scopes the workspace's data by workspace id", () => {
    expect(auroraKeys.skills("ws-1")).toEqual(["aurora", "ws-1", "skills"]);
    expect(auroraKeys.assets("ws-1")).toEqual(["aurora", "ws-1", "assets"]);
    expect(auroraKeys.generation("ws-1", "gen-1")).toEqual([
      "aurora",
      "ws-1",
      "generations",
      "gen-1",
    ]);
  });

  it("sits under the generation prefix so one invalidation sweeps list and detail", () => {
    expect(auroraKeys.generation("ws-1", "gen-1").slice(0, 3)).toEqual(
      auroraKeys.generations("ws-1"),
    );
    expect(auroraKeys.generationList("ws-1").slice(0, 3)).toEqual(
      auroraKeys.generations("ws-1"),
    );
    expect(auroraKeys.assetList("ws-1").slice(0, 3)).toEqual(
      auroraKeys.assets("ws-1"),
    );
  });

  it("keys a page by its params so it cannot be served another page's rows", () => {
    expect(auroraKeys.generationList("ws-1")).toEqual([
      "aurora",
      "ws-1",
      "generations",
      "list",
      {},
    ]);
    expect(auroraKeys.generationList("ws-1", { limit: 5 })).toEqual([
      "aurora",
      "ws-1",
      "generations",
      "list",
      { limit: 5 },
    ]);
    expect(auroraKeys.assetList("ws-1", { generationId: "gen-1" })).toEqual([
      "aurora",
      "ws-1",
      "assets",
      { generationId: "gen-1" },
    ]);
  });
});

describe("auroraWalletKeys", () => {
  it("keeps the account-level wallet out of the workspace subtree", () => {
    // The wallet belongs to the user, not the workspace — every workspace shows
    // the same balance — so its key must not be reachable by invalidating one
    // workspace's data, and it must not be refetched on a workspace switch.
    expect(auroraKeys.all("ws-1")).toEqual(["aurora", "ws-1"]);
    expect(auroraWalletKeys.balance().slice(0, 2)).toEqual(["aurora", "wallet"]);
    expect(auroraWalletKeys.transactions().slice(0, 2)).toEqual(["aurora", "wallet"]);
  });
});
