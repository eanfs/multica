// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  auroraAssetSchema,
  auroraBalanceSchema,
  auroraCheckoutResponseSchema,
  auroraGenerationDetailSchema,
  auroraGenerationSchema,
  auroraSkillsSchema,
  auroraSubscriptionSchema,
  auroraTopupsSchema,
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

describe("auroraSubscriptionSchema", () => {
  it("defaults every field so a degraded read still renders a plan", () => {
    // The plan card is the first thing the billing screen draws; a body that
    // predates the endpoint must produce a free plan, not a blank card.
    const res = auroraSubscriptionSchema.parse({});

    expect(res.tier).toBe("free");
    expect(res.status).toBe("");
    expect(res.currentPeriodEnd).toBeNull();
    expect(res.cancelAtPeriodEnd).toBe(false);
    expect(res.limits).toEqual({ generationsPerMonth: 10, concurrency: 1 });
    expect(res.usage).toEqual({
      generationsUsedThisMonth: 0,
      activeGenerations: 0,
    });
  });

  it("keeps a plan and its usage as they arrive", () => {
    const res = auroraSubscriptionSchema.parse({
      tier: "creator",
      status: "active",
      currentPeriodEnd: "2026-10-24T00:00:00Z",
      cancelAtPeriodEnd: true,
      limits: { generationsPerMonth: 30, concurrency: 2 },
      usage: { generationsUsedThisMonth: 12, activeGenerations: 1 },
    });

    expect(res.tier).toBe("creator");
    expect(res.currentPeriodEnd).toBe("2026-10-24T00:00:00Z");
    expect(res.limits.generationsPerMonth).toBe(30);
    expect(res.usage.generationsUsedThisMonth).toBe(12);
  });

  it("keeps a status this build has never seen", () => {
    // The server sends the status as a plain string; a new one must reach the
    // screen so its label mapping can fall back, not fail the parse.
    const res = auroraSubscriptionSchema.parse({ status: "paused" });

    expect(res.status).toBe("paused");
  });
});

describe("auroraTopupsSchema", () => {
  it("defaults a missing list to empty", () => {
    expect(auroraTopupsSchema.parse({}).topups).toEqual([]);
  });

  it("reads the packs", () => {
    const res = auroraTopupsSchema.parse({
      topups: [{ id: "t5", credits: 5000 }],
    });

    expect(res.topups[0]?.id).toBe("t5");
    expect(res.topups[0]?.credits).toBe(5000);
  });
});

describe("auroraCheckoutResponseSchema", () => {
  it("rejects a body with no checkout URL", () => {
    // No default here on purpose: an empty URL would navigate the user back to
    // the page they are already on, as if the purchase had completed.
    expect(auroraCheckoutResponseSchema.safeParse({}).success).toBe(false);
  });

  it("reads the URL the browser is sent to", () => {
    expect(
      auroraCheckoutResponseSchema.parse({
        checkoutUrl: "https://checkout.stripe.com/c/test",
      }).checkoutUrl,
    ).toBe("https://checkout.stripe.com/c/test");
  });
});
