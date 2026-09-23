import type { SupportedLocale } from "@multica/core/i18n";

// Same map as apps/web/lib/html-lang.ts — keep the two in step; the font stacks
// in each app's globals.css branch on these values.
//
// HTML lang uses BCP-47 region tags widely recognized by screen readers and
// font stacks. i18next keeps zh-Hans internally because that is the resource
// key, while the document uses zh-CN for accessibility and CJK fallback.
export const HTML_LANG: Record<SupportedLocale, string> = {
  en: "en",
  "zh-Hans": "zh-CN",
  ko: "ko-KR",
  ja: "ja-JP",
  fr: "fr-FR",
};
