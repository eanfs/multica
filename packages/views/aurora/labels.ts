/**
 * How the catalog's and the server's vocabulary reaches the screen.
 *
 * Two things live here, both of them about picking a label rather than
 * rendering one:
 *
 *  - The server enums, mapped onto the locale keys that name them. Both are
 *    deliberately open on the wire — the schemas keep them `z.string()` so a
 *    value from a newer server still parses — so each mapping ends in a
 *    fallback instead of assuming this build has seen every value. Without it a
 *    new status would render as an undefined label rather than as "Unknown".
 *  - The catalog's bilingual skill names, picked for the reader's language.
 *
 * Each mapping returns a *key segment*, not copy. Callers index the bundle with
 * it under the namespace that owns the label: `aurora.composer.status.*` for a
 * generation status, `billing.transaction.kind.*` for a ledger kind.
 */

import type { AuroraSkill } from "@multica/core/aurora";

/** Where a generation is, as `aurora.composer.status.*`. */
export type AuroraGenerationStatusLabel =
  | "queued"
  | "running"
  | "completed"
  | "failed"
  | "unknown";

const GENERATION_STATUS_LABELS: Record<string, AuroraGenerationStatusLabel> = {
  queued: "queued",
  running: "running",
  completed: "completed",
  failed: "failed",
};

/**
 * The status key for a generation.
 *
 * The server collapses the lifecycle it reads from the enqueued task into these
 * four (`aurora.go`), and a cancelled task reports as `failed`.
 */
export function generationStatusLabel(
  status: string | undefined,
): AuroraGenerationStatusLabel {
  return GENERATION_STATUS_LABELS[status ?? ""] ?? "unknown";
}

/** How a wallet moved, as `billing.transaction.kind.*`. */
export type AuroraLedgerKindLabel =
  | "topup"
  | "deduction"
  | "refund"
  | "adjustment"
  | "expire"
  | "unknown";

const LEDGER_KIND_LABELS: Record<string, AuroraLedgerKindLabel> = {
  topup: "topup",
  deduction: "deduction",
  refund: "refund",
  adjustment: "adjustment",
  // Plan 5's monthly settlement writes this; the server's kind list
  // (`aurora/credit.go`) documents it as unreleased, so it is only ever seen
  // once that lands.
  expire: "expire",
};

/**
 * The label key for a ledger row's kind.
 *
 * This is the fallback path for a transaction the screen cannot attribute to a
 * generation — a grant, a plan key, or anything a later release writes.
 */
export function ledgerKindLabel(
  kind: string | undefined,
): AuroraLedgerKindLabel {
  return LEDGER_KIND_LABELS[kind ?? ""] ?? "unknown";
}

/**
 * The name to put on a skill card, for the language the UI is in.
 *
 * The catalog carries exactly two: a Chinese `name` and an English `nameEn`
 * (`aurora/catalog.go`). A Chinese reader gets the Chinese name and every other
 * reader gets the English one, which is the catalog's own convention rather
 * than a translation we could add here. `nameEn` defaults to `""` in the
 * schema, so a Chinese name is still the better answer than an empty label.
 */
export function skillDisplayName(skill: AuroraSkill, locale: string): string {
  if (locale.startsWith("zh")) return skill.name;
  return skill.nameEn || skill.name;
}

/** A directory category tab, as `aurora.directory.categories.*`. */
export type AuroraCategoryLabel =
  | "image"
  | "video"
  | "content"
  | "office"
  | "other";

const CATEGORY_LABELS: Record<string, AuroraCategoryLabel> = {
  image: "image",
  video: "video",
  content: "content",
  office: "office",
};

/**
 * The label key for a catalog category.
 *
 * The catalog ships four (`aurora/catalog.go`), but `category` stays a plain
 * string on the wire, so a fifth one gets a tab labelled generically rather
 * than one showing its raw server value.
 */
export function auroraCategoryLabel(category: string): AuroraCategoryLabel {
  return CATEGORY_LABELS[category] ?? "other";
}
