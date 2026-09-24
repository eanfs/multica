import type { ZodType } from "zod";
import { type Logger, noopLogger } from "../logger";

// Module-level logger for schema warnings. Defaults to no-op so test
// runs don't spam stderr; the platform layer wires a real logger via
// `setSchemaLogger` at app boot.
let schemaLogger: Logger = noopLogger;

export function setSchemaLogger(logger: Logger): void {
  schemaLogger = logger;
}

export interface ParseOptions {
  /** Endpoint identifier used in the warning log so we can grep for which
   *  contract drifted in production telemetry. */
  endpoint: string;
}

/**
 * A validated value, plus whether it came from a body the schema accepted.
 *
 * `parseWithFallback` collapses both outcomes into `T`, which is what a caller
 * that only needs something renderable wants. A caller for which the fallback
 * would be a *claim* rather than a placeholder — "your wallet holds 0", "this
 * workspace has no skills" — needs to tell the two apart, and reads the flag
 * here instead of guessing from an empty list.
 */
export interface ParseResult<T> {
  /** The parsed value, or `fallback` when the body failed validation. */
  value: T;
  /** True when `value` is the fallback rather than the server's body. */
  degraded: boolean;
}

/**
 * Validate a JSON value parsed from an API response against a zod schema,
 * returning the parsed value plus whether it was actually readable.
 *
 * On failure we log a warning with the endpoint and zod's structured error,
 * but never throw — the UI layer must keep rendering. This is the boundary
 * defense that turns "API contract drifted" from a white-screen incident
 * into a degraded-but-rendering page.
 *
 * The return type is anchored to `T` (inferred from `fallback`), not to the
 * schema's `z.infer` type. Schemas are intentionally **lenient** — string
 * enums kept as `z.string()` so an unknown enum value still parses, etc. —
 * so the parsed runtime value can be wider than the strict TS type at the
 * call site. The caller asserts compatibility by typing the fallback to the
 * expected `T`; downstream code is already responsible for handling unknown
 * enum values via `default`-bearing switches and optional chaining.
 *
 * See CLAUDE.md "API Response Compatibility" for when to reach for this.
 */
export function parseWithFallbackResult<T>(
  data: unknown,
  schema: ZodType,
  fallback: T,
  opts: ParseOptions,
): ParseResult<T> {
  const result = schema.safeParse(data);
  if (result.success) return { value: result.data as T, degraded: false };
  schemaLogger.warn(
    `API response failed schema validation: ${opts.endpoint}`,
    {
      endpoint: opts.endpoint,
      issues: result.error.issues,
      received: data,
    },
  );
  return { value: fallback, degraded: true };
}

/**
 * The same validation, collapsed to the value.
 *
 * This is the shape almost every caller wants: something of the declared type
 * to render, whether or not the body behind it was readable. It stays the
 * narrow `T` it always was, so a caller that cannot act on degradation is not
 * made to carry a flag it would ignore — reach for `parseWithFallbackResult`
 * when the fallback would otherwise read as a fact.
 */
export function parseWithFallback<T>(
  data: unknown,
  schema: ZodType,
  fallback: T,
  opts: ParseOptions,
): T {
  return parseWithFallbackResult(data, schema, fallback, opts).value;
}
