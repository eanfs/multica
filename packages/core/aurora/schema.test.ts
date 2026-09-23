// @vitest-environment node
import { describe, expect, it } from "vitest";
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

  it("rejects an entry that is missing a required field", () => {
    // `credits` is required — the composer prices a run from it, so an entry
    // without one is not usable. zod rejects the whole array rather than the
    // single entry, which is the documented trade-off: one drifted row degrades
    // the directory to empty rather than rendering a skill that cannot be
    // priced. api.test.ts owns the other half — that the caller sees [].
    const res = auroraSkillsSchema.safeParse({
      skills: [{ id: "poster", name: "海报制作", category: "image" }],
    });

    expect(res.success).toBe(false);
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
});

describe("auroraTransactionsSchema", () => {
  it("defaults the optional ledger columns", () => {
    const res = auroraTransactionsSchema.parse({
      transactions: [{ id: "tx-1", kind: "deduction", amountMicro: -760 }],
    });

    expect(res.transactions[0]?.balanceAfterMicro).toBe(0);
    expect(res.transactions[0]?.reference).toBe("");
    expect(res.transactions[0]?.createdAt).toBe("");
  });
});
