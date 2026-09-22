// @vitest-environment node
import { describe, expect, it } from "vitest";
import { auroraGenerationDetailOptions, auroraKeys, auroraWalletKeys } from "./queries";
import { AURORA_GENERATION_POLL_MS } from "./types";

/**
 * Pulls the polling decision out of the options object so the wiring can be
 * pinned without fake timers. A `false` answer is what stops the poll; a number
 * is the next tick.
 *
 * Only the wiring is asserted here. Which *statuses* are final is
 * `isAuroraGenerationTerminal`'s contract, and types.test.ts owns that matrix —
 * re-listing it would mean two files to edit when the server's vocabulary
 * grows.
 */
function pollInterval(query: {
  status: string;
  data: { status: string } | null;
}): number | false {
  const options = auroraGenerationDetailOptions("ws-1", "gen-1");
  const refetchInterval = options.refetchInterval as unknown as (query: {
    state: { status: string; data: { status: string } | null };
  }) => number | false;
  return refetchInterval({ state: query });
}

describe("auroraGenerationDetailOptions", () => {
  it("polls a generation that is still in flight", () => {
    expect(
      pollInterval({ status: "success", data: { status: "running" } }),
    ).toBe(AURORA_GENERATION_POLL_MS);
  });

  it("keeps polling when the detail body could not be read", () => {
    // null is what parseAuroraGenerationDetail falls back to; it is not a
    // finished generation, so the screen keeps trying rather than parking.
    expect(pollInterval({ status: "success", data: null })).toBe(
      AURORA_GENERATION_POLL_MS,
    );
  });

  it("stops polling once the generation is final", () => {
    expect(
      pollInterval({ status: "success", data: { status: "completed" } }),
    ).toBe(false);
  });

  it("stops polling a failed read instead of retrying it forever", () => {
    // `refetchInterval` ignores query status, so without this an id that 404s
    // would be re-requested every 3s for as long as the screen is open.
    expect(pollInterval({ status: "error", data: null })).toBe(false);
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
