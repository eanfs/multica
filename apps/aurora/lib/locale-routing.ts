import {
  matchLocale,
  SUPPORTED_LOCALES,
  type SupportedLocale,
} from "@multica/core/i18n";

/**
 * Request header proxy.ts stamps with the resolved locale so the RSC layout can
 * read it without re-deriving from cookies and `accept-language`.
 *
 * Private to this app: the proxy writes it and the layout reads it on the same
 * request, so there is nothing to share across apps. The resolution below
 * mirrors apps/web/lib/locale-routing.ts — same signals, same precedence.
 */
export const MULTICA_LOCALE_HEADER = "x-multica-locale";

export function isSupportedLocale(
  value: string | null,
): value is SupportedLocale {
  return (
    value !== null && (SUPPORTED_LOCALES as readonly string[]).includes(value)
  );
}

export function resolveLocaleFromSignals({
  cookieLocale,
  acceptLanguage,
}: {
  cookieLocale?: string | null;
  acceptLanguage?: string | null;
}): SupportedLocale {
  const candidates: string[] = [];
  if (cookieLocale) candidates.push(cookieLocale);

  for (const part of (acceptLanguage ?? "").split(",")) {
    const tag = part.split(";")[0]?.trim();
    if (tag) candidates.push(tag);
  }

  return matchLocale(candidates);
}
