// @vitest-environment node
import { beforeEach, describe, expect, it } from "vitest";
import { ApiError, setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import {
  auroraAssetDownloadPath,
  createAuroraCheckout,
  createAuroraGeneration,
  createAuroraTopupCheckout,
  deleteAuroraAsset,
  getAuroraSubscription,
  isAuroraCheckoutConflictError,
  isAuroraDegraded,
  isAuroraInsufficientCreditsError,
  isAuroraPaymentsUnavailableError,
  isAuroraRateLimitError,
  listAuroraAssets,
  listAuroraGenerations,
  listAuroraSkills,
  listAuroraTopups,
  parseAuroraAssets,
  parseAuroraBalance,
  parseAuroraCheckout,
  parseAuroraGeneration,
  parseAuroraGenerationDetail,
  parseAuroraGenerations,
  parseAuroraSkills,
  parseAuroraSubscription,
  parseAuroraTopupCheckout,
  parseAuroraTopups,
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

    expect(skills.degraded).toBe(false);
    expect(skills.value).toHaveLength(1);
    expect(skills.value[0]?.nameEn).toBe("Poster");
    expect(skills.value[0]?.credits).toBe(760);
  });

  it("degrades a non-array body to an empty directory, flagged as degraded", () => {
    expect(parseAuroraSkills({ skills: "not-an-array" })).toEqual({
      value: [],
      degraded: true,
    });
    expect(parseAuroraSkills(null)).toEqual({ value: [], degraded: true });
  });

  it("degrades an entry missing a required field to an empty directory", () => {
    // schema.test.ts owns why zod rejects the whole entry; this is the half the
    // directory screen depends on.
    expect(
      parseAuroraSkills({
        skills: [{ id: "poster", name: "海报制作", category: "image" }],
      }),
    ).toEqual({ value: [], degraded: true });
  });

  it("parses the policy's attachment rules and defaults them for an older server", () => {
    const withRules = parseAuroraSkills({
      skills: [
        {
          id: "poster",
          name: "海报制作",
          category: "image",
          credits: 760,
          attachment_rules: [
            { kinds: ["image"], min: 0, max: 4, max_bytes: 26_214_400 },
          ],
        },
      ],
    });

    expect(withRules.value[0]?.attachments).toEqual([
      { kinds: ["image"], min: 0, max: 4, maxBytes: 26_214_400 },
    ]);

    // A server that predates the field must still parse: the composer treats an
    // absent rule set as "this skill takes no attachments".
    const withoutRules = parseAuroraSkills({
      skills: [
        { id: "text-image", name: "文字生成图片", category: "image", credits: 680 },
      ],
    });

    expect(withoutRules.value[0]?.attachments).toEqual([]);
  });

  it("tells a degraded catalog apart from an empty one (GH #55)", () => {
    // The directory renders "no skills" for both without the flag, and only one
    // of them is a fact.
    const empty = parseAuroraSkills({ skills: [] });
    const degraded = parseAuroraSkills({ skills: "not-an-array" });

    expect(empty.value).toEqual(degraded.value);
    expect(isAuroraDegraded(empty)).toBe(false);
    expect(isAuroraDegraded(degraded)).toBe(true);
  });
});

describe("parseAuroraGenerations", () => {
  it("degrades a non-array body to an empty list, flagged as degraded", () => {
    expect(parseAuroraGenerations({ generations: 42 })).toEqual({
      value: [],
      degraded: true,
    });
  });
});

describe("parseAuroraGeneration", () => {
  it("unwraps the create response", () => {
    const parsed = parseAuroraGeneration({
      generation: {
        id: "gen-1",
        skillId: "poster",
        prompt: "a cat",
        status: "queued",
        creditsReserved: 760,
      },
    });

    expect(parsed.degraded).toBe(false);
    expect(parsed.value?.id).toBe("gen-1");
    expect(parsed.value?.creditsReserved).toBe(760);
  });

  it("reads an unreadable body as null rather than an empty generation", () => {
    // A placeholder id here would send the composer to a detail screen for a
    // generation that does not exist. `degraded` is what the composer turns into
    // "check My works before starting another" instead of a retry.
    expect(parseAuroraGeneration({ generation: { id: "gen-1" } })).toEqual({
      value: null,
      degraded: true,
    });
    expect(parseAuroraGeneration("not-an-object")).toEqual({
      value: null,
      degraded: true,
    });
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

    expect(detail.degraded).toBe(false);
    expect(detail.value?.status).toBe("completed");
    expect(detail.value?.assets).toHaveLength(1);
    expect(detail.value?.assets[0]?.mediaUrl).toBe("https://cdn.example/a.png");
  });

  it("reads an unreadable body as a degraded null", () => {
    expect(
      parseAuroraGenerationDetail({ generation: "not-an-object" }),
    ).toEqual({ value: null, degraded: true });
  });
});

describe("parseAuroraAssets", () => {
  it("degrades a non-array body to an empty library, flagged as degraded", () => {
    expect(parseAuroraAssets({ assets: "not-an-array" })).toEqual({
      value: [],
      degraded: true,
    });
  });
});

describe("parseAuroraBalance and parseAuroraTransactions", () => {
  it("reads the wallet and the ledger", () => {
    expect(parseAuroraBalance({ availableMicro: 12_000_000 })).toEqual({
      value: { availableMicro: 12_000_000 },
      degraded: false,
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
      }).value,
    ).toHaveLength(1);
  });

  it("degrades unreadable bodies to zero and to an empty ledger", () => {
    expect(parseAuroraBalance("not-an-object").value.availableMicro).toBe(0);
    expect(parseAuroraTransactions({ transactions: 7 }).value).toEqual([]);
  });

  it("flags a degraded wallet so its 0 cannot be read as a real balance", () => {
    // The sharpest case in GH #55: a corrupted body parses to a *number*, and
    // the billing screen printed it as "0 credits" — a claim that sends the user
    // to top up against a figure the client never read.
    const broke = parseAuroraBalance({ availableMicro: 0 });
    const degraded = parseAuroraBalance("not-an-object");

    expect(degraded.value).toEqual(broke.value);
    expect(isAuroraDegraded(broke)).toBe(false);
    expect(isAuroraDegraded(degraded)).toBe(true);
  });

  it("treats a missing payload as not degraded", () => {
    // A query that has not resolved has no payload to judge; the caller reads
    // that as pending, not as a corrupted body.
    expect(isAuroraDegraded(undefined)).toBe(false);
  });
});

describe("parseAuroraSubscription", () => {
  it("reads the plan the server reports", () => {
    const res = parseAuroraSubscription({
      subscription: { tier: "pro", status: "active" },
    });

    expect(res.tier).toBe("pro");
    expect(res.status).toBe("active");
  });

  it("rejects an unreadable body instead of inventing a free plan", () => {
    // Billing state controls a real purchase. A paid user must not see a
    // purchase-capable Free card because the response contract degraded.
    expect(() =>
      parseAuroraSubscription({ subscription: "nope" }),
    ).toThrow("invalid subscription response");
  });
});

describe("parseAuroraTopups", () => {
  it("rejects an unreadable body instead of claiming no packs are sold", () => {
    expect(() => parseAuroraTopups({ topups: 7 })).toThrow(
      "invalid topup response",
    );
  });

  it("reads the packs", () => {
    expect(
      parseAuroraTopups({ topups: [{ id: "t20", credits: 20000 }] }),
    ).toEqual([{ id: "t20", credits: 20000 }]);
  });
});

describe("parseAuroraCheckout", () => {
  it("reads the checkout URL", () => {
    expect(
      parseAuroraCheckout({ checkoutUrl: "https://checkout.stripe.com/c/1" }),
    ).toBe("https://checkout.stripe.com/c/1");
  });

  it("rejects an unreadable body instead of reporting a successful checkout", () => {
    expect(() => parseAuroraCheckout({})).toThrow("invalid checkout response");
    expect(() => parseAuroraTopupCheckout({ checkoutUrl: 7 })).toThrow(
      "invalid checkout response",
    );
  });

  it("rejects checkout destinations that are not absolute HTTPS URLs", () => {
    expect(() =>
      parseAuroraCheckout({ checkoutUrl: "http://checkout.stripe.test/c/1" }),
    ).toThrow("invalid checkout response");
    expect(() =>
      parseAuroraCheckout({ checkoutUrl: "javascript:alert(1)" }),
    ).toThrow("invalid checkout response");
    expect(() => parseAuroraCheckout({ checkoutUrl: "/checkout/1" })).toThrow(
      "invalid checkout response",
    );
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

  it("posts the composer's request body, including attachment ids, as JSON", async () => {
    const requests = installFakeClient({ generation: null });

    await createAuroraGeneration({
      skillId: "poster",
      prompt: "a cat",
      attachmentIds: ["att-1", "att-2"],
    });

    expect(requests[0]?.path).toBe("/api/aurora/generations");
    expect(requests[0]?.init?.method).toBe("POST");
    expect(requests[0]?.init?.body).toBe(
      JSON.stringify({
        skillId: "poster",
        prompt: "a cat",
        attachmentIds: ["att-1", "att-2"],
      }),
    );
  });

  it("reads the plan and the topup list from the billing endpoints", async () => {
    let requests = installFakeClient({ subscription: { tier: "free" } });

    await getAuroraSubscription();

    expect(requests[0]?.path).toBe("/api/aurora/billing/subscription");

    requests = installFakeClient({ topups: [] });
    await listAuroraTopups();
    expect(requests[0]?.path).toBe("/api/aurora/billing/topups");
  });

  it("posts the checkout bodies as JSON", async () => {
    const returnURLs = {
      successUrl: "https://app.example.com/acme/billing?checkout=success",
      cancelUrl: "https://app.example.com/acme/billing?checkout=cancel",
    };
    let requests = installFakeClient({ checkoutUrl: "https://stripe.test/1" });

    await createAuroraCheckout({
      tier: "creator",
      billingCycle: "monthly",
      ...returnURLs,
    });

    expect(requests[0]?.path).toBe("/api/aurora/billing/checkout");
    expect(requests[0]?.init?.method).toBe("POST");
    expect(requests[0]?.init?.body).toBe(
      JSON.stringify({
        tier: "creator",
        billingCycle: "monthly",
        ...returnURLs,
      }),
    );

    requests = installFakeClient({ checkoutUrl: "https://stripe.test/2" });
    await createAuroraTopupCheckout({ topupId: "t5", ...returnURLs });
    expect(requests[0]?.path).toBe("/api/aurora/billing/topup/checkout");
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

  it("reads 409 as an existing subscription", () => {
    expect(
      isAuroraCheckoutConflictError(
        new ApiError("subscription already exists", 409, "Conflict"),
      ),
    ).toBe(true);
  });

  it("reads 503 as a deployment without payments", () => {
    expect(
      isAuroraPaymentsUnavailableError(
        new ApiError("payments not configured", 503, "Unavailable"),
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
    expect(isAuroraCheckoutConflictError(new Error("network"))).toBe(false);
    expect(
      isAuroraCheckoutConflictError(new ApiError("boom", 503, "Unavailable")),
    ).toBe(false);
    expect(
      isAuroraPaymentsUnavailableError(new ApiError("boom", 409, "Conflict")),
    ).toBe(false);
  });
});
