"use client";

import { Coins, Images, LogOut, Sparkles } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { cn } from "@multica/ui/lib/utils";
import { AppLink, useNavigation } from "@multica/views/navigation";
import { useT } from "@multica/views/i18n";
import { useLogout } from "@multica/views/auth";
import { auroraRoutes } from "@/lib/routes";

/**
 * The app's own chrome: three destinations, one account action.
 *
 * A destination is a link, never a button — the shell is the only navigator in
 * Aurora, and a plain anchor keeps middle-click, "copy link" and the browser's
 * own history semantics working. Labels come from the `aurora` namespace so the
 * nav and the page it opens always name the same thing.
 */
export function AuroraShell({
  slug,
  children,
}: {
  slug: string;
  children: React.ReactNode;
}) {
  const { t } = useT("aurora");
  const { t: tLayout } = useT("layout");
  const { pathname } = useNavigation();
  const logout = useLogout();
  const routes = auroraRoutes(slug);

  const items = [
    { href: routes.skills(), icon: Sparkles, label: t(($) => $.directory.title) },
    { href: routes.works(), icon: Images, label: t(($) => $.works.title) },
    { href: routes.billing(), icon: Coins, label: t(($) => $.billing.title) },
  ];
  // Trailing slashes are the same destination, and Next keeps the URL the user
  // typed. Comparing raw strings would drop the selected state for `…/works/`.
  const current = pathname.replace(/\/+$/, "");

  return (
    <div className="flex h-svh min-h-0">
      <nav
        aria-label="Aurora"
        className="flex w-56 shrink-0 flex-col border-r border-sidebar-border bg-sidebar"
      >
        <p className="px-4 py-4 text-title-sm font-semibold text-sidebar-foreground">
          Aurora
        </p>
        <ul className="flex flex-col gap-0.5 px-2">
          {items.map(({ href, icon: Icon, label }) => {
            const selected = current === href;
            return (
              <li key={href}>
                <AppLink
                  href={href}
                  aria-current={selected ? "page" : undefined}
                  className={cn(
                    "flex items-center gap-2 rounded-md px-2 py-1.5 text-body text-sidebar-foreground/80",
                    // The selected fill stays heavier than the hover fill so a
                    // pointer resting on the current page cannot be mistaken for
                    // a move to it.
                    selected
                      ? "bg-sidebar-accent font-medium text-sidebar-accent-foreground"
                      : "hover:bg-sidebar-accent/60 hover:text-sidebar-accent-foreground",
                  )}
                >
                  <Icon aria-hidden="true" className="size-4 shrink-0" />
                  <span className="truncate">{label}</span>
                </AppLink>
              </li>
            );
          })}
        </ul>
        <div className="mt-auto p-2">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="w-full justify-start gap-2"
            onClick={logout}
          >
            <LogOut aria-hidden="true" className="size-4 shrink-0" />
            {tLayout(($) => $.sidebar.log_out)}
          </Button>
        </div>
      </nav>
      <main className="min-w-0 flex-1 overflow-hidden">{children}</main>
    </div>
  );
}
