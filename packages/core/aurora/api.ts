import { ApiError, api } from "../api";
import { parseWithFallback } from "../api/schema";
import {
  auroraAssetsSchema,
  auroraBalanceSchema,
  auroraCheckoutResponseSchema,
  auroraGenerationDetailSchema,
  auroraGenerationResponseSchema,
  auroraGenerationsSchema,
  auroraSkillsSchema,
  auroraSubscriptionResponseSchema,
  auroraSubscriptionSchema,
  auroraTopupsSchema,
  auroraTransactionsSchema,
  type AuroraAsset,
  type AuroraBalance,
  type AuroraCheckout,
  type AuroraGeneration,
  type AuroraGenerationDetail,
  type AuroraSkill,
  type AuroraSubscription,
  type AuroraTopup,
  type AuroraTransaction,
} from "./schema";
import type {
  AuroraAssetsParams,
  AuroraListParams,
  CreateAuroraCheckoutRequest,
  CreateAuroraGenerationRequest,
  CreateAuroraTopupCheckoutRequest,
} from "./types";

/**
 * Aurora's network surface.
 *
 * Routes and response schemas are the two halves of one contract, so they live
 * together here and in ./schema rather than being split between
 * `api/client.ts` and `api/schemas.ts` the way the older domains are. The
 * shared client still owns the transport: `api.requestJson` carries the auth
 * and CSRF headers, the CSRF retry, the 401 path and the structured `ApiError`.
 *
 * Each `parseAurora*` below is the schema plus its fallback, so a malformed
 * body degrades to an empty list, a zero balance, or `null` for a single
 * entity — never a thrown parse error and never a blank screen.
 */

const AURORA_SKILLS_PATH = "/api/aurora/skills";
const AURORA_GENERATIONS_PATH = "/api/aurora/generations";
const AURORA_ASSETS_PATH = "/api/aurora/assets";
const AURORA_BALANCE_PATH = "/api/aurora/billing/balance";
const AURORA_TRANSACTIONS_PATH = "/api/aurora/billing/transactions";
const AURORA_SUBSCRIPTION_PATH = "/api/aurora/billing/subscription";
const AURORA_TOPUPS_PATH = "/api/aurora/billing/topups";
const AURORA_CHECKOUT_PATH = "/api/aurora/billing/checkout";
const AURORA_TOPUP_CHECKOUT_PATH = "/api/aurora/billing/topup/checkout";

// The label `parseWithFallback` logs, built from the path it describes so the
// two cannot drift. The detail route carries its `{id}` placeholder rather than
// a value, so a log line groups every generation read instead of one per id.
const SKILLS_ENDPOINT = `GET ${AURORA_SKILLS_PATH}`;
const GENERATIONS_ENDPOINT = `GET ${AURORA_GENERATIONS_PATH}`;
const GENERATION_ENDPOINT = `GET ${AURORA_GENERATIONS_PATH}/{id}`;
const CREATE_GENERATION_ENDPOINT = `POST ${AURORA_GENERATIONS_PATH}`;
const ASSETS_ENDPOINT = `GET ${AURORA_ASSETS_PATH}`;
const BALANCE_ENDPOINT = `GET ${AURORA_BALANCE_PATH}`;
const TRANSACTIONS_ENDPOINT = `GET ${AURORA_TRANSACTIONS_PATH}`;
const SUBSCRIPTION_ENDPOINT = `GET ${AURORA_SUBSCRIPTION_PATH}`;
const TOPUPS_ENDPOINT = `GET ${AURORA_TOPUPS_PATH}`;
const CHECKOUT_ENDPOINT = `POST ${AURORA_CHECKOUT_PATH}`;
const TOPUP_CHECKOUT_ENDPOINT = `POST ${AURORA_TOPUP_CHECKOUT_PATH}`;

/**
 * Renders the list params the server understands as a URL suffix — `""` when
 * there are none, otherwise a leading `?`. Absent params are skipped, so a
 * request carries only the filters the caller actually set.
 */
function querySuffix(
  params?: Record<string, string | number | undefined>,
): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params ?? {})) {
    if (value !== undefined && value !== "") search.set(key, String(value));
  }
  const encoded = search.toString();
  return encoded ? `?${encoded}` : "";
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// Each fallback is built at the call site rather than shared as a module
// constant: `parseWithFallback` returns the fallback by reference, so one
// shared array would be aliased by every degraded cache entry — and a single
// in-place mutation of any of them would corrupt all the others.

/** The skill catalog, or an empty directory when the body is unreadable. */
export function parseAuroraSkills(data: unknown): AuroraSkill[] {
  return parseWithFallback<{ skills: AuroraSkill[] }>(
    data,
    auroraSkillsSchema,
    { skills: [] },
    { endpoint: SKILLS_ENDPOINT },
  ).skills;
}

/**
 * The generation created by `POST /api/aurora/generations`, or null when the
 * body is unreadable.
 *
 * Null rather than an empty generation: the id is the only handle the caller
 * has on what was just created, and a placeholder id would send the composer
 * to a detail screen for something that does not exist.
 */
export function parseAuroraGeneration(
  data: unknown,
): AuroraGeneration | null {
  return (
    parseWithFallback<{ generation: AuroraGeneration } | null>(
      data,
      auroraGenerationResponseSchema,
      null,
      { endpoint: CREATE_GENERATION_ENDPOINT },
    )?.generation ?? null
  );
}

/** One page of the workspace's generations, or an empty list. */
export function parseAuroraGenerations(data: unknown): AuroraGeneration[] {
  return parseWithFallback<{ generations: AuroraGeneration[] }>(
    data,
    auroraGenerationsSchema,
    { generations: [] },
    { endpoint: GENERATIONS_ENDPOINT },
  ).generations;
}

/** One generation with its assets, or null when the body is unreadable. */
export function parseAuroraGenerationDetail(
  data: unknown,
): AuroraGenerationDetail | null {
  return (
    parseWithFallback<{ generation: AuroraGenerationDetail } | null>(
      data,
      auroraGenerationDetailSchema,
      null,
      { endpoint: GENERATION_ENDPOINT },
    )?.generation ?? null
  );
}

/** The workspace's asset library, or an empty library. */
export function parseAuroraAssets(data: unknown): AuroraAsset[] {
  return parseWithFallback<{ assets: AuroraAsset[] }>(
    data,
    auroraAssetsSchema,
    { assets: [] },
    { endpoint: ASSETS_ENDPOINT },
  ).assets;
}

/** The caller's wallet, or a zero balance the next refetch will correct. */
export function parseAuroraBalance(data: unknown): AuroraBalance {
  return parseWithFallback<AuroraBalance>(
    data,
    auroraBalanceSchema,
    { availableMicro: 0 },
    { endpoint: BALANCE_ENDPOINT },
  );
}

/** The caller's ledger, newest first, or an empty ledger. */
export function parseAuroraTransactions(data: unknown): AuroraTransaction[] {
  return parseWithFallback<{ transactions: AuroraTransaction[] }>(
    data,
    auroraTransactionsSchema,
    { transactions: [] },
    { endpoint: TRANSACTIONS_ENDPOINT },
  ).transactions;
}

/**
 * The caller's plan.
 *
 * Optional fields still receive schema defaults for compatibility with older
 * servers, but a body that cannot identify a subscription at all is not safe to
 * turn into Free: this screen can start a real recurring purchase. The unique
 * fallback object lets us retain parseWithFallback's boundary diagnostics while
 * promoting degradation to a visible query error.
 */
export function parseAuroraSubscription(data: unknown): AuroraSubscription {
  const fallback = { subscription: auroraSubscriptionSchema.parse({}) };
  const parsed = parseWithFallback<{ subscription: AuroraSubscription }>(
    data,
    auroraSubscriptionResponseSchema,
    fallback,
    { endpoint: SUBSCRIPTION_ENDPOINT },
  );
  if (parsed === fallback) throw new Error("invalid subscription response");
  return parsed.subscription;
}

/** The purchasable credit packs; an unreadable catalog is a visible error. */
export function parseAuroraTopups(data: unknown): AuroraTopup[] {
  const fallback: { topups: AuroraTopup[] } = { topups: [] };
  const parsed = parseWithFallback<{ topups: AuroraTopup[] }>(
    data,
    auroraTopupsSchema,
    fallback,
    { endpoint: TOPUPS_ENDPOINT },
  );
  if (parsed === fallback) throw new Error("invalid topup response");
  return parsed.topups;
}

/**
 * The validated HTTPS checkout URL from a started purchase.
 *
 * A malformed response throws: checkout is a command, and silently returning a
 * placeholder would leave the button with neither navigation nor an error.
 */
export function parseAuroraCheckout(data: unknown): string {
  return parseCheckoutUrl(data, CHECKOUT_ENDPOINT);
}

/** The same body shape, logged under the topup endpoint it came from. */
export function parseAuroraTopupCheckout(data: unknown): string {
  return parseCheckoutUrl(data, TOPUP_CHECKOUT_ENDPOINT);
}

function parseCheckoutUrl(data: unknown, endpoint: string): string {
  const parsed = parseWithFallback<AuroraCheckout | null>(
    data,
    auroraCheckoutResponseSchema,
    null,
    { endpoint },
  );
  if (!parsed) {
    // A checkout is a command, not a readable projection: treating a malformed
    // 200 body as a successful null result leaves the button with no navigation
    // and no error. Throw so the mutation enters its visible failure state.
    throw new Error("invalid checkout response");
  }
  return parsed.checkoutUrl;
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

/** The catalog, including the phase-2 skills that cannot be run yet. */
export async function listAuroraSkills(): Promise<AuroraSkill[]> {
  return parseAuroraSkills(await api.requestJson(AURORA_SKILLS_PATH));
}

/**
 * Enqueues a generation and reserves its credits.
 *
 * Throws `ApiError` with a 402 when the wallet cannot cover the skill — the
 * caller shows the top-up prompt — and 429 for the monthly quota, the
 * concurrency cap or the per-user frequency gate. Both are distinguishable
 * with `isAuroraInsufficientCreditsError` / `isAuroraRateLimitError`.
 */
export async function createAuroraGeneration(
  request: CreateAuroraGenerationRequest,
): Promise<AuroraGeneration | null> {
  const raw = await api.requestJson(AURORA_GENERATIONS_PATH, {
    method: "POST",
    body: JSON.stringify(request),
  });
  return parseAuroraGeneration(raw);
}

/** The workspace's generations, newest first. */
export async function listAuroraGenerations(
  params?: AuroraListParams,
): Promise<AuroraGeneration[]> {
  const raw = await api.requestJson(
    `${AURORA_GENERATIONS_PATH}${querySuffix(params)}`,
  );
  return parseAuroraGenerations(raw);
}

/** One generation with its assets — the progress screen's polling source. */
export async function getAuroraGeneration(
  id: string,
): Promise<AuroraGenerationDetail | null> {
  const raw = await api.requestJson(
    `${AURORA_GENERATIONS_PATH}/${encodeURIComponent(id)}`,
  );
  return parseAuroraGenerationDetail(raw);
}

/** The workspace's content library, optionally narrowed to one generation. */
export async function listAuroraAssets(
  params?: AuroraAssetsParams,
): Promise<AuroraAsset[]> {
  const raw = await api.requestJson(
    `${AURORA_ASSETS_PATH}${querySuffix(params)}`,
  );
  return parseAuroraAssets(raw);
}

/** Removes an asset and the object behind it. The server answers 204. */
export async function deleteAuroraAsset(assetId: string): Promise<void> {
  await api.requestJson(
    `${AURORA_ASSETS_PATH}/${encodeURIComponent(assetId)}`,
    { method: "DELETE" },
  );
}

/**
 * The URL the browser downloads one asset from.
 *
 * The server answers with a redirect to a short-lived signed URL, or streams
 * the file itself where no URL can be signed, so this is a link target rather
 * than something to fetch — the same shape as `attachmentDownloadPath`.
 */
export function auroraAssetDownloadPath(assetId: string): string {
  return `${AURORA_ASSETS_PATH}/${encodeURIComponent(assetId)}/download`;
}

/** The caller's wallet. Every workspace shows the same balance. */
export async function getAuroraBalance(): Promise<AuroraBalance> {
  return parseAuroraBalance(await api.requestJson(AURORA_BALANCE_PATH));
}

/** The caller's ledger, newest first. */
export async function listAuroraTransactions(): Promise<AuroraTransaction[]> {
  return parseAuroraTransactions(
    await api.requestJson(AURORA_TRANSACTIONS_PATH),
  );
}

/** The caller's plan and this month's usage. Every workspace shows the same. */
export async function getAuroraSubscription(): Promise<AuroraSubscription> {
  return parseAuroraSubscription(
    await api.requestJson(AURORA_SUBSCRIPTION_PATH),
  );
}

/** The credit packs this deployment sells. */
export async function listAuroraTopups(): Promise<AuroraTopup[]> {
  return parseAuroraTopups(await api.requestJson(AURORA_TOPUPS_PATH));
}

/**
 * Starts a subscription checkout and returns the URL to send the browser to.
 *
 * Throws `ApiError` with 409 when the caller already has a live subscription,
 * and 503 when the deployment has no Stripe configuration — neither is a state
 * the client can retry its way out of, so the caller reports it rather than
 * retrying.
 */
export async function createAuroraCheckout(
  request: CreateAuroraCheckoutRequest,
): Promise<string> {
  const raw = await api.requestJson(AURORA_CHECKOUT_PATH, {
    method: "POST",
    body: JSON.stringify(request),
  });
  return parseAuroraCheckout(raw);
}

/** Starts a one-time credit purchase and returns the checkout URL. */
export async function createAuroraTopupCheckout(
  request: CreateAuroraTopupCheckoutRequest,
): Promise<string> {
  const raw = await api.requestJson(AURORA_TOPUP_CHECKOUT_PATH, {
    method: "POST",
    body: JSON.stringify(request),
  });
  return parseAuroraTopupCheckout(raw);
}

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

/**
 * 402: the wallet cannot cover the skill's credits, so nothing was reserved and
 * nothing was enqueued. The composer turns this into the top-up prompt instead
 * of a generic failure.
 */
export function isAuroraInsufficientCreditsError(error: unknown): boolean {
  return error instanceof ApiError && error.status === 402;
}

/**
 * 429: one of the three server gates refused the request — the monthly
 * generation quota, the concurrency cap, or the per-user frequency limit. They
 * share a status and differ only in the server's message, which is not a
 * contract, so callers surface one "limit reached, try again later" line
 * rather than guessing which gate fired.
 */
export function isAuroraRateLimitError(error: unknown): boolean {
  return error instanceof ApiError && error.status === 429;
}

/**
 * 409: a checkout was refused because the caller already has an active or
 * past-due subscription. Retrying cannot succeed, and the plan screen says why
 * rather than reporting a generic failure.
 */
export function isAuroraCheckoutConflictError(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409;
}

/**
 * 503: this deployment has no payment provider or no price for the requested
 * plan. Not retryable, and not the user's fault — the screen says payments are
 * unavailable rather than asking them to try again.
 */
export function isAuroraPaymentsUnavailableError(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503;
}
