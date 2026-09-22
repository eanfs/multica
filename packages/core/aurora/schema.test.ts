// @vitest-environment node
import { describe, expect, it } from "vitest";
import { parseWithFallback } from "../api/schema";
import {
  auroraAssetSchema,
  auroraBalanceSchema,
  auroraGenerationDetailSchema,
  auroraGenerationSchema,
  auroraSkillsSchema,
  auroraTransactionsSchema,
} from "./schema";

describe("auroraSkillsSchema", () => {
  it("fills in the optional fields a minimal entry omits", () => {
    const res = auroraSkillsSchema.parse({
      skills: [
        { id: "poster", name: "海报制作", credits: 760, category: "image" },
      ],
    });

    expect(res.skills[0]?.nameEn).toBe("");
    expect(res.skills[0]?.input).toEqual([]);
    expect(res.skills[0]?.output).toEqual([]);
    expect(res.skills[0]?.featured).toBe(false);
    expect(res.skills[0]?.available).toBe(true);
  });

  it("maps the wire's name_en onto nameEn", () => {
    const res = auroraSkillsSchema.parse({
      skills: [
        {
          id: "poster",
          name: "海报制作",
          name_en: "Poster",
          credits: 760,
          category: "image",
        },
      ],
    });

    expect(res.skills[0]?.nameEn).toBe("Poster");
  });

  it("falls back to an empty list on a non-array body", () => {
    const res = parseWithFallback(
      { skills: "not-an-array" },
      auroraSkillsSchema,
      { skills: [] },
      { endpoint: "GET /api/aurora/skills" },
    );

    expect(res.skills).toEqual([]);
  });

  it("falls back to an empty list when a required field is missing", () => {
    // `credits` is required — the composer prices a run from it, so a entry
    // without one is not usable. zod rejects the whole array rather than the
    // entry, which is the documented trade-off: one drifted row degrades the
    // directory to empty rather than rendering a skill that cannot be priced.
    const res = parseWithFallback(
      { skills: [{ id: "poster", name: "海报制作", category: "image" }] },
      auroraSkillsSchema,
      { skills: [] },
      { endpoint: "GET /api/aurora/skills" },
    );

    expect(res.skills).toEqual([]);
  });
});

describe("auroraGenerationSchema", () => {
  it("defaults creditsReserved and keeps an unknown status parseable", () => {
    const res = auroraGenerationSchema.parse({
      id: "gen-1",
      skillId: "poster",
      prompt: "a cat",
      status: "some-future-status",
    });

    expect(res.creditsReserved).toBe(0);
    expect(res.status).toBe("some-future-status");
  });

  it("rejects an entry missing its id", () => {
    const res = auroraGenerationSchema.safeParse({
      skillId: "poster",
      prompt: "a cat",
      status: "queued",
    });

    expect(res.success).toBe(false);
  });
});

describe("auroraGenerationDetailSchema", () => {
  it("defaults a missing assets list to empty", () => {
    const res = auroraGenerationDetailSchema.parse({
      generation: {
        id: "gen-1",
        skillId: "poster",
        prompt: "a cat",
        status: "running",
      },
    });

    expect(res.generation.assets).toEqual([]);
  });
});

describe("auroraAssetSchema", () => {
  it("defaults the nullable and timestamp columns", () => {
    const res = auroraAssetSchema.parse({
      id: "asset-1",
      generationId: "gen-1",
      kind: "image",
    });

    expect(res.mediaUrl).toBeNull();
    expect(res.format).toBeNull();
    expect(res.createdAt).toBe("");
  });

  it("keeps an explicitly null mediaUrl usable", () => {
    const res = auroraAssetSchema.parse({
      id: "asset-1",
      generationId: "gen-1",
      kind: "image",
      mediaUrl: null,
      format: "png",
      createdAt: "2026-09-22T00:00:00Z",
    });

    expect(res.mediaUrl).toBeNull();
    expect(res.format).toBe("png");
  });
});

describe("auroraBalanceSchema", () => {
  it("defaults a missing balance to zero", () => {
    expect(auroraBalanceSchema.parse({}).availableMicro).toBe(0);
  });

  it("falls back to a zero balance when the body is a string", () => {
    const res = parseWithFallback(
      "not-an-object",
      auroraBalanceSchema,
      { availableMicro: 0 },
      { endpoint: "GET /api/aurora/billing/balance" },
    );

    expect(res.availableMicro).toBe(0);
  });
});

describe("auroraTransactionsSchema", () => {
  it("falls back to an empty ledger on a non-array body", () => {
    const res = parseWithFallback(
      { transactions: null },
      auroraTransactionsSchema,
      { transactions: [] },
      { endpoint: "GET /api/aurora/billing/transactions" },
    );

    expect(res.transactions).toEqual([]);
  });

  it("defaults the optional ledger columns", () => {
    const res = auroraTransactionsSchema.parse({
      transactions: [{ id: "tx-1", kind: "deduction", amountMicro: -760 }],
    });

    expect(res.transactions[0]?.balanceAfterMicro).toBe(0);
    expect(res.transactions[0]?.reference).toBe("");
    expect(res.transactions[0]?.createdAt).toBe("");
  });
});
