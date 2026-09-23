// @vitest-environment node
import { beforeEach, describe, expect, it } from "vitest";
import { ApiError, setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import {
  auroraAssetDownloadPath,
  createAuroraGeneration,
  deleteAuroraAsset,
  isAuroraInsufficientCreditsError,
  isAuroraRateLimitError,
  listAuroraAssets,
  listAuroraGenerations,
  listAuroraSkills,
  parseAuroraAssets,
  parseAuroraBalance,
  parseAuroraGeneration,
  parseAuroraGenerationDetail,
  parseAuroraGenerations,
  parseAuroraSkills,
  parseAuroraTransactions,
} from "./api";

/** Records the request each endpoint builds, and answers with a fixed body. */
function installFakeClient(body: unknown) {
  const requests: Array<{ path: string; init?: RequestInit }> = [];
  setApiInstance({
    requestJson: async (path: string, init?: RequestInit) => {
      requests.push({ path, init });
      return body;
    },
  } as unknown as ApiClient);
  return requests;
}

describe("parseAuroraSkills", () => {
  it("returns the catalog when the body matches", () => {
    const skills = parseAuroraSkills({
      skills: [
        {
          id: "poster",
          name: "海报制作",
          name_en: "Poster",
          category: "image",
          credits: 760,
          input: ["text", "image"],
          output: ["image"],
          featured: true,
          available: true,
        },
      ],
    });

    expect(skills).toHaveLength(1);
    expect(skills[0]?.nameEn).toBe("Poster");
    expect(skills[0]?.credits).toBe(760);
  });

  it("degrades a non-array body to an empty directory", () => {
    expect(parseAuroraSkills({ skills: "not-an-array" })).toEqual([]);
    expect(parseAuroraSkills(null)).toEqual([]);
  });

  it("degrades an entry missing a required field to an empty directory", () => {
    // schema.test.ts owns why zod rejects the whole entry; this is the half the
    // directory screen depends on.
    expect(
      parseAuroraSkills({
        skills: [{ id: "poster", name: "海报制作", category: "image" }],
      }),
    ).toEqual([]);
  });
});

describe("parseAuroraGenerations", () => {
  it("degrades a non-array body to an empty list", () => {
    expect(parseAuroraGenerations({ generations: 42 })).toEqual([]);
  });
});

describe("parseAuroraGeneration", () => {
  it("unwraps the create response", () => {
    const generation = parseAuroraGeneration({
      generation: {
        id: "gen-1",
        skillId: "poster",
        prompt: "a cat",
        status: "queued",
        creditsReserved: 760,
      },
    });

    expect(generation?.id).toBe("gen-1");
    expect(generation?.creditsReserved).toBe(760);
  });

  it("reads an unreadable body as null rather than an empty generation", () => {
    // A placeholder id here would send the composer to a detail screen for a
    // generation that does not exist.
    expect(parseAuroraGeneration({ generation: { id: "gen-1" } })).toBeNull();
    expect(parseAuroraGeneration("not-an-object")).toBeNull();
  });
});

describe("parseAuroraGenerationDetail", () => {
  it("returns the generation with its assets", () => {
    const detail = parseAuroraGenerationDetail({
      generation: {
        id: "gen-1",
        skillId: "poster",
        prompt: "a cat",
        status: "completed",
        creditsReserved: 760,
        assets: [
          {
            id: "asset-1",
            generationId: "gen-1",
            kind: "image",
            mediaUrl: "https://cdn.example/a.png",
            format: "png",
            createdAt: "2026-09-22T00:00:00Z",
          },
        ],
      },
    });

    expect(detail?.status).toBe("completed");
    expect(detail?.assets).toHaveLength(1);
    expect(detail?.assets[0]?.mediaUrl).toBe("https://cdn.example/a.png");
  });

  it("reads an unreadable body as null", () => {
    expect(parseAuroraGenerationDetail({ generation: "not-an-object" })).toBeNull();
  });
});

describe("parseAuroraAssets", () => {
  it("degrades a non-array body to an empty library", () => {
    expect(parseAuroraAssets({ assets: "not-an-array" })).toEqual([]);
  });
});

describe("parseAuroraBalance and parseAuroraTransactions", () => {
  it("reads the wallet and the ledger", () => {
    expect(parseAuroraBalance({ availableMicro: 12_000_000 })).toEqual({
      availableMicro: 12_000_000,
    });
    expect(
      parseAuroraTransactions({
        transactions: [
          {
            id: "tx-1",
            kind: "deduction",
            amountMicro: -760_000_000,
            balanceAfterMicro: 4_000_000,
            reference: "gen-1",
            createdAt: "2026-09-22T00:00:00Z",
          },
        ],
      }),
    ).toHaveLength(1);
  });

  it("degrades unreadable bodies to zero and to an empty ledger", () => {
    expect(parseAuroraBalance("not-an-object").availableMicro).toBe(0);
    expect(parseAuroraTransactions({ transactions: 7 })).toEqual([]);
  });
});

describe("request paths", () => {
  beforeEach(() => {
    setApiInstance(null as unknown as ApiClient);
  });

  it("reads the catalog from the skills endpoint", async () => {
    const requests = installFakeClient({ skills: [] });

    await listAuroraSkills();

    expect(requests).toHaveLength(1);
    expect(requests[0]?.path).toBe("/api/aurora/skills");
    expect(requests[0]?.init).toBeUndefined();
  });

  it("passes the generation list's paging through as query params", async () => {
    const requests = installFakeClient({ generations: [] });

    await listAuroraGenerations({ limit: 5, offset: 10 });

    expect(requests[0]?.path).toBe("/api/aurora/generations?limit=5&offset=10");
  });

  it("omits absent query params instead of sending empty ones", async () => {
    const requests = installFakeClient({ assets: [] });

    await listAuroraAssets({ generationId: "gen-1" });

    expect(requests[0]?.path).toBe("/api/aurora/assets?generationId=gen-1");
  });

  it("posts the composer's request body as JSON", async () => {
    const requests = installFakeClient({ generation: null });

    await createAuroraGeneration({ skillId: "poster", prompt: "a cat" });

    expect(requests[0]?.path).toBe("/api/aurora/generations");
    expect(requests[0]?.init?.method).toBe("POST");
    expect(requests[0]?.init?.body).toBe(
      JSON.stringify({ skillId: "poster", prompt: "a cat" }),
    );
  });

  it("deletes an asset with no body to read", async () => {
    const requests = installFakeClient(undefined);

    await expect(deleteAuroraAsset("asset-1")).resolves.toBeUndefined();

    expect(requests[0]?.path).toBe("/api/aurora/assets/asset-1");
    expect(requests[0]?.init?.method).toBe("DELETE");
  });
});

describe("auroraAssetDownloadPath", () => {
  it("builds the download path the browser follows", () => {
    expect(auroraAssetDownloadPath("asset-1")).toBe(
      "/api/aurora/assets/asset-1/download",
    );
  });
});

describe("error classification", () => {
  it("reads 402 as an insufficient wallet", () => {
    expect(
      isAuroraInsufficientCreditsError(
        new ApiError("insufficient credits", 402, "Payment Required"),
      ),
    ).toBe(true);
  });

  it("reads 429 as one of the server's limits", () => {
    expect(
      isAuroraRateLimitError(
        new ApiError("too many requests", 429, "Too Many Requests"),
      ),
    ).toBe(true);
  });

  it("does not claim unrelated failures", () => {
    expect(isAuroraInsufficientCreditsError(new Error("network"))).toBe(false);
    expect(isAuroraRateLimitError(new ApiError("boom", 500, "Server Error"))).toBe(
      false,
    );
    expect(isAuroraInsufficientCreditsError(new ApiError("boom", 429, "Rate"))).toBe(
      false,
    );
  });
});
