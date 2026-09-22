import { ApiError, api } from "../api";
import { parseWithFallback } from "../api/schema";
import {
  auroraAssetsSchema,
  auroraBalanceSchema,
  auroraGenerationDetailSchema,
  auroraGenerationResponseSchema,
  auroraGenerationsSchema,
  auroraSkillsSchema,
  auroraTransactionsSchema,
  type AuroraAsset,
  type AuroraBalance,
  type AuroraGeneration,
  type AuroraGenerationDetail,
  type AuroraSkill,
  type AuroraTransaction,
} from "./schema";
import type {
  AuroraAssetsParams,
  AuroraListParams,
  CreateAuroraGenerationRequest,
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

// The label `parseWithFallback` logs, built from the path it describes so the
// two cannot drift. The detail route carries its `{id}` placeholder rather than
// a value, so a log line groups every generation read instead of one per id.
const SKILLS_ENDPOINT = `GET ${AURORA_SKILLS_PATH}`;
const GENERATIONS_ENDPOINT = `GET ${AURORA_GENERATIONS_PATH}`;
const GENERATION_ENDPOINT = "GET /api/aurora/generations/{id}";
const CREATE_GENERATION_ENDPOINT = `POST ${AURORA_GENERATIONS_PATH}`;
const ASSETS_ENDPOINT = `GET ${AURORA_ASSETS_PATH}`;
const BALANCE_ENDPOINT = `GET ${AURORA_BALANCE_PATH}`;
const TRANSACTIONS_ENDPOINT = `GET ${AURORA_TRANSACTIONS_PATH}`;

/**
 * Serialises the list params the server understands, skipping the absent ones
 * so a request carries only the filters the caller actually set.
 */
function queryString(
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

const EMPTY_SKILLS: { skills: AuroraSkill[] } = { skills: [] };
const EMPTY_GENERATIONS: { generations: AuroraGeneration[] } = {
  generations: [],
};
const EMPTY_ASSETS: { assets: AuroraAsset[] } = { assets: [] };
const EMPTY_TRANSACTIONS: { transactions: AuroraTransaction[] } = {
  transactions: [],
};
const EMPTY_BALANCE: AuroraBalance = { availableMicro: 0 };

/** The skill catalog, or an empty directory when the body is unreadable. */
export function parseAuroraSkills(data: unknown): AuroraSkill[] {
  return parseWithFallback(data, auroraSkillsSchema, EMPTY_SKILLS, {
    endpoint: SKILLS_ENDPOINT,
  }).skills;
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
  return parseWithFallback(data, auroraGenerationsSchema, EMPTY_GENERATIONS, {
    endpoint: GENERATIONS_ENDPOINT,
  }).generations;
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
  return parseWithFallback(data, auroraAssetsSchema, EMPTY_ASSETS, {
    endpoint: ASSETS_ENDPOINT,
  }).assets;
}

/** The caller's wallet, or a zero balance the next refetch will correct. */
export function parseAuroraBalance(data: unknown): AuroraBalance {
  return parseWithFallback(data, auroraBalanceSchema, EMPTY_BALANCE, {
    endpoint: BALANCE_ENDPOINT,
  });
}

/** The caller's ledger, newest first, or an empty ledger. */
export function parseAuroraTransactions(data: unknown): AuroraTransaction[] {
  return parseWithFallback(data, auroraTransactionsSchema, EMPTY_TRANSACTIONS, {
    endpoint: TRANSACTIONS_ENDPOINT,
  }).transactions;
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
    `${AURORA_GENERATIONS_PATH}${queryString(params)}`,
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
    `${AURORA_ASSETS_PATH}${queryString(params)}`,
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
