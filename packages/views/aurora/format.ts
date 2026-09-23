/**
 * Credit presentation for the Aurora views.
 *
 * The wire carries micro-credits everywhere — `availableMicro`, `amountMicro`,
 * `balanceAfterMicro`, `creditsReserved` — while the catalog and the product
 * copy talk in whole credits (`microCreditsPerCredit`, `server/internal/handler/aurora.go`).
 * Every conversion between the two goes through here so a screen cannot
 * disagree with the one next to it about what a balance is.
 */

/** The ledger's unit: 1 credit = 1e6 micro-credits. */
export const MICRO_CREDITS_PER_CREDIT = 1_000_000;

/**
 * Micro-credits as a whole credit count, rounded.
 *
 * Rounding rather than truncating because a generation settles the credits it
 * actually used, which is rarely a whole multiple of a credit; truncation
 * would report a settled charge as the credit below it.
 */
export function microToCredits(micro: number): number {
  return Math.round(micro / MICRO_CREDITS_PER_CREDIT);
}

/**
 * A credit count grouped for the reader's locale, e.g. `"1,234"`.
 *
 * The locale is required rather than defaulting to the runtime's, so a Chinese
 * UI in an English-language browser groups the way the text around it reads —
 * the same reason `useLocale()` exists and every caller passes it.
 */
export function formatCredits(credits: number, locale: string): string {
  return new Intl.NumberFormat(locale).format(credits);
}

/**
 * A micro-credit amount as grouped whole credits — the conversion and the
 * grouping together, so a screen cannot apply one without the other.
 */
export function formatMicroCredits(micro: number, locale: string): string {
  return formatCredits(microToCredits(micro), locale);
}
