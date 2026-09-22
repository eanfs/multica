/**
 * The part of Aurora's type surface that is not derived from a wire schema.
 *
 * The entity shapes — `AuroraSkill`, `AuroraGeneration`, `AuroraAsset`,
 * `AuroraGenerationDetail`, `AuroraBalance`, `AuroraTransaction` — are inferred
 * from the zod schemas in ./schema, because that is where the wire contract
 * lives. Both are re-exported from ./index, so consumers import from
 * `@multica/core/aurora`. What is left here is the vocabulary the hooks need:
 * request bodies, list params, and the generation status rules the progress
 * poll keys on.
 */

/** Body of `POST /api/aurora/generations`. */
export interface CreateAuroraGenerationRequest {
  skillId: string;
  prompt: string;
}

/**
 * Paging for the generation and asset lists. Both are optional: the server
 * falls back to its own defaults (50 rows from offset 0) for a missing,
 * unparseable or out-of-range value, so a client bug cannot turn a list into a
 * 400.
 *
 * Declared as aliases rather than interfaces so they stay assignable to the
 * `Record<string, ...>` that ./api serialises query params with — an interface
 * has no implicit index signature.
 */
export type AuroraListParams = {
  limit?: number;
  offset?: number;
};

/** Paging for the asset library, plus its one filter. */
export type AuroraAssetsParams = AuroraListParams & {
  /** Narrows the library to one generation. Omitted lists the whole workspace. */
  generationId?: string;
};

/**
 * How often an unfinished generation is re-read, in ms.
 *
 * The MVP polls rather than subscribes: a generation's status is derived
 * server-side from the task it enqueued (`aurora.go`), and the `task:progress`
 * realtime frame is not produced for it yet. Three seconds keeps a
 * composer-to-result flow feeling live; one in-flight generation costs one
 * request per three seconds until it settles.
 */
export const AURORA_GENERATION_POLL_MS = 3_000;

/**
 * Whether the server has stopped working on this generation.
 *
 * Only "completed" and "failed" are final — the server collapses queued,
 * dispatched, running, waiting and deferred into queued/running, and a
 * cancelled task into failed. The check is a positive list rather than a
 * negation of "queued"/"running" on purpose: an unrecognised status from a
 * newer server would otherwise read as final and stop the poll in the window
 * before the generation's assets existed.
 */
export function isAuroraGenerationTerminal(status: string | undefined): boolean {
  return status === "completed" || status === "failed";
}
