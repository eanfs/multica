import { z } from "zod";

/**
 * Wire schemas for the Aurora API (`server/internal/handler/aurora.go`,
 * `aurora_artifact.go`).
 *
 * They are deliberately lenient, in the shape CLAUDE.md's API-compatibility
 * rules ask for: fields an older server may omit carry defaults, nullable
 * columns are nullable rather than optional, and enum-ish columns stay
 * `z.string()` so a value from a newer server still parses. A body that is not
 * a shape at all (a list where an object belongs, a string where a list
 * belongs) fails the parse and is handled by the `parseAurora*` functions in
 * ./api, which route every response through `parseWithFallback`.
 */

/**
 * One catalog entry. The catalog is the single source of truth for the
 * directory, the composer's credit price, and whether a skill can run.
 *
 * The wire names the English label `name_en` (`aurora/catalog.go`) while the
 * rest of the API is camelCase; the transform maps it to `nameEn` so consumers
 * see one naming convention.
 */
export const auroraSkillSchema = z
  .object({
    id: z.string(),
    name: z.string(),
    name_en: z.string().default(""),
    category: z.string(),
    credits: z.number(),
    input: z.array(z.string()).default([]),
    output: z.array(z.string()).default([]),
    featured: z.boolean().default(false),
    /** false = a phase-2 skill: listed, but not yet runnable. */
    available: z.boolean().default(true),
  })
  .transform(({ name_en, ...skill }) => ({ ...skill, nameEn: name_en }));
export type AuroraSkill = z.infer<typeof auroraSkillSchema>;

export const auroraSkillsSchema = z.object({
  skills: z.array(auroraSkillSchema),
});

export const auroraGenerationSchema = z.object({
  id: z.string(),
  skillId: z.string(),
  prompt: z.string(),
  /**
   * "queued" | "running" | "completed" | "failed", derived server-side from
   * the enqueued task. Kept as a plain string so an unrecognised value from a
   * newer server still parses — consumers switch on it with a `default` branch
   * and decide terminality with `isAuroraGenerationTerminal` (./types).
   */
  status: z.string(),
  creditsReserved: z.number().default(0),
});
export type AuroraGeneration = z.infer<typeof auroraGenerationSchema>;

/** POST /api/aurora/generations answers with the generation under a wrapper. */
export const auroraGenerationResponseSchema = z.object({
  generation: auroraGenerationSchema,
});

export const auroraGenerationsSchema = z.object({
  generations: z.array(auroraGenerationSchema),
});

export const auroraAssetSchema = z.object({
  id: z.string(),
  generationId: z.string(),
  kind: z.string(),
  /** A row can exist before its object does; both are usable as "no file". */
  mediaUrl: z.string().nullable().default(null),
  format: z.string().nullable().default(null),
  createdAt: z.string().default(""),
});
export type AuroraAsset = z.infer<typeof auroraAssetSchema>;

/**
 * GET /api/aurora/generations/{id}. The detail response is the summary plus
 * the generation's own assets, which is why the library list has no separate
 * per-generation query.
 */
export const auroraGenerationDetailSchema = z.object({
  generation: auroraGenerationSchema.extend({
    assets: z.array(auroraAssetSchema).default([]),
  }),
});
export type AuroraGenerationDetail = z.infer<
  typeof auroraGenerationDetailSchema
>["generation"];

export const auroraAssetsSchema = z.object({
  assets: z.array(auroraAssetSchema),
});

/** GET /api/aurora/billing/balance — the caller's wallet, in micro-credits. */
export const auroraBalanceSchema = z.object({
  availableMicro: z.number().default(0),
});
export type AuroraBalance = z.infer<typeof auroraBalanceSchema>;

export const auroraTransactionSchema = z.object({
  id: z.string(),
  kind: z.string(),
  /** Signed: a reservation is negative, a refund or grant positive. */
  amountMicro: z.number(),
  balanceAfterMicro: z.number().default(0),
  /** The row's subject — a generation id, or a grant/plan key. */
  reference: z.string().default(""),
  createdAt: z.string().default(""),
});
export type AuroraTransaction = z.infer<typeof auroraTransactionSchema>;

export const auroraTransactionsSchema = z.object({
  transactions: z.array(auroraTransactionSchema).default([]),
});

/**
 * GET /api/aurora/billing/subscription.
 *
 * `tier` is the tier the server's gates actually apply — a canceled plan
 * reports the free tier here — while `status` is the stored subscription
 * status and is empty for a user with no plan row at all. The two together are
 * what let the screen say "Free" without claiming a canceled plan is still
 * running.
 *
 * Every field carries a default because the endpoint is the one the plan screen
 * renders first: a server that predates it (or a degraded body) must still
 * produce a free-plan card rather than a blank screen.
 */
export const auroraSubscriptionSchema = z.object({
  tier: z.string().default("free"),
  status: z.string().default(""),
  currentPeriodEnd: z.string().nullable().default(null),
  cancelAtPeriodEnd: z.boolean().default(false),
  limits: z
    .object({
      generationsPerMonth: z.number().default(10),
      concurrency: z.number().default(1),
    })
    .default({ generationsPerMonth: 10, concurrency: 1 }),
  usage: z
    .object({
      generationsUsedThisMonth: z.number().default(0),
      activeGenerations: z.number().default(0),
    })
    .default({ generationsUsedThisMonth: 0, activeGenerations: 0 }),
});
export type AuroraSubscription = z.infer<typeof auroraSubscriptionSchema>;

export const auroraSubscriptionResponseSchema = z.object({
  subscription: auroraSubscriptionSchema,
});

/** GET /api/aurora/billing/topups — the purchasable credit packs. */
export const auroraTopupSchema = z.object({
  id: z.string(),
  credits: z.number(),
});
export type AuroraTopup = z.infer<typeof auroraTopupSchema>;

export const auroraTopupsSchema = z.object({
  topups: z.array(auroraTopupSchema).default([]),
});

/**
 * The checkout endpoints' shared answer: a URL to send the browser to. No
 * default — a body without a URL means the checkout did not start, and a
 * placeholder URL would navigate the user somewhere meaningless.
 */
export const auroraCheckoutResponseSchema = z.object({
  checkoutUrl: z.string(),
});
export type AuroraCheckout = z.infer<typeof auroraCheckoutResponseSchema>;
