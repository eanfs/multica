import {
  render,
  type RenderOptions,
  type RenderResult,
} from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { SupportedLocale } from "@multica/core/i18n";
import { RESOURCES } from "@multica/views/locales";
import type { ReactElement, ReactNode } from "react";

/**
 * Render a component under the production resource map, so a test that reaches
 * for a namespace the app does not ship fails loudly instead of rendering keys
 * as text. Pass `locale` to assert localized copy; the default is "en".
 *
 * The equivalent lives at packages/views/test/i18n.tsx, which is not reachable
 * from here: that package's `exports` map names its production entry points
 * only, so an app cannot import its test helpers.
 */
export function renderWithI18n(
  ui: ReactElement,
  options: Omit<RenderOptions, "wrapper"> & { locale?: SupportedLocale } = {},
): RenderResult {
  const { locale = "en", ...rest } = options;
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <I18nProvider locale={locale} resources={RESOURCES}>
        {children}
      </I18nProvider>
    );
  }
  return render(ui, { wrapper: Wrapper, ...rest });
}
