"use client";

import { useMemo } from "react";
import { CreditCard, Frown, Wallet } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import {
  useAuroraBalance,
  useAuroraGenerations,
  useAuroraSkills,
  useAuroraTransactions,
  type AuroraTransaction,
} from "@multica/core/aurora";
import {
  CollectionPageHeader,
  CollectionPageState,
} from "../layout/collection-page";
import { AppLink } from "../navigation";
import { useLocale, useT } from "../i18n";
import { formatCredits, microToCredits } from "./format";
import {
  ledgerKindLabel,
  skillDisplayName,
  type AuroraLedgerKindLabel,
} from "./labels";

/**
 * The wallet: what is left, how it moved, and the way to add more.
 *
 * Every number the ledger carries is a micro-credit, so the conversion to
 * credits happens here rather than at each call site — a screen that showed a
 * raw micro-credit would be off by a factor of a million from the catalog it
 * prices against.
 */
export interface AuroraBillingProps {
  /**
   * The app's checkout route. Omitted, the entry renders disabled with the
   * reason, rather than pointing at a route that does not exist yet.
   */
  topUpHref?: string;
}

export function AuroraBilling({ topUpHref }: AuroraBillingProps = {}) {
  const { t } = useT("aurora");
  const { t: tBilling } = useT("billing");
  const locale = useLocale();
  const balanceQuery = useAuroraBalance();
  const transactionsQuery = useAuroraTransactions();
  const generationsQuery = useAuroraGenerations();
  const skillsQuery = useAuroraSkills();

  const transactions = transactionsQuery.data ?? [];

  // A ledger row's `reference` is a generation id for a charge or its refund,
  // and a grant key (Plan 5 adds `sub:` and `signup:`) for everything else. Only
  // the first kind can be resolved back to something a reader recognises, so the
  // two maps below are built once and both misses fall through to the kind.
  const { skillIdByGeneration, skillNameById } = useMemo(() => {
    const skillIdByGeneration = new Map<string, string>();
    for (const generation of generationsQuery.data ?? []) {
      skillIdByGeneration.set(generation.id, generation.skillId);
    }
    const skillNameById = new Map<string, string>();
    for (const skill of skillsQuery.data ?? []) {
      skillNameById.set(skill.id, skillDisplayName(skill, locale));
    }
    return { skillIdByGeneration, skillNameById };
  }, [generationsQuery.data, skillsQuery.data, locale]);

  function referenceSkillName(transaction: AuroraTransaction): string | null {
    const skillId = skillIdByGeneration.get(transaction.reference);
    if (!skillId) return null;
    return skillNameById.get(skillId) ?? null;
  }

  const isLoading = balanceQuery.isPending || transactionsQuery.isPending;
  const loadFailed =
    transactionsQuery.isError ||
    (balanceQuery.isError && balanceQuery.data === undefined);

  const balance = formatCredits(
    microToCredits(balanceQuery.data?.availableMicro ?? 0),
    locale,
  );

  return (
    <div className="flex h-full min-h-0 flex-col">
      <CollectionPageHeader
        icon={Wallet}
        title={t(($) => $.billing.title)}
      />

      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
        {isLoading ? (
          <BillingSkeleton />
        ) : loadFailed ? (
          <CollectionPageState
            icon={Frown}
            tone="destructive"
            role="alert"
            title={t(($) => $.billing.load_failed_title)}
            description={t(($) => $.billing.load_failed_description)}
            actions={
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => {
                  void balanceQuery.refetch();
                  void transactionsQuery.refetch();
                }}
              >
                {t(($) => $.retry)}
              </Button>
            }
          />
        ) : (
          <div className="flex flex-col gap-6">
            <section className="flex flex-col gap-1 rounded-lg border border-surface-border p-4">
              <span className="text-caption text-muted-foreground">
                {t(($) => $.billing.balance_title)}
              </span>
              <span className="font-mono text-2xl tabular-nums">{balance}</span>
              {topUpHref ? (
                <AppLink
                  href={topUpHref}
                  className="mt-1 inline-flex w-fit items-center gap-1 text-body text-muted-foreground transition-colors hover:text-foreground"
                >
                  <CreditCard aria-hidden="true" className="size-3.5" />
                  {t(($) => $.billing.top_up)}
                </AppLink>
              ) : (
                <div className="mt-1 flex flex-wrap items-center gap-2">
                  <Button type="button" variant="outline" size="sm" disabled>
                    {t(($) => $.billing.top_up)}
                  </Button>
                  <span className="text-caption text-muted-foreground">
                    {t(($) => $.billing.top_up_placeholder)}
                  </span>
                </div>
              )}
            </section>

            <section className="flex flex-col gap-2">
              <h2 className="text-label font-medium">
                {t(($) => $.billing.transactions_title)}
              </h2>
              {transactions.length === 0 ? (
                <CollectionPageState
                  icon={Wallet}
                  title={t(($) => $.billing.transactions_empty_title)}
                  description={t(
                    ($) => $.billing.transactions_empty_description,
                  )}
                />
              ) : (
                <ul className="rounded-lg border border-surface-border">
                  {transactions.map((transaction) => (
                    <TransactionRow
                      key={transaction.id}
                      transaction={transaction}
                      // A row the catalog can attribute names the skill; one it
                      // cannot shows only the kind, which is the same label the
                      // reader would have fallen back to anyway.
                      skillName={referenceSkillName(transaction)}
                      kindLabel={tBilling(
                        ($) =>
                          $.transaction.kind[
                            ledgerKindLabel(
                              transaction.kind,
                            ) as AuroraLedgerKindLabel
                          ],
                      )}
                    />
                  ))}
                </ul>
              )}
            </section>
          </div>
        )}
      </div>
    </div>
  );
}

function TransactionRow({
  transaction,
  skillName,
  kindLabel,
}: {
  transaction: AuroraTransaction;
  skillName: string | null;
  kindLabel: string;
}) {
  const { t } = useT("aurora");
  const locale = useLocale();
  const credits = microToCredits(transaction.amountMicro);
  const amount =
    credits < 0
      ? t(($) => $.billing.amount_negative, {
          amount: formatCredits(Math.abs(credits), locale),
        })
      : t(($) => $.billing.amount_positive, {
          amount: formatCredits(credits, locale),
        });

  return (
    <li className="flex items-center gap-3 border-b border-surface-border px-3 py-2 last:border-b-0">
      <div className="flex min-w-0 flex-1 flex-col">
        <span className="truncate text-body">{kindLabel}</span>
        {skillName ? (
          <span className="truncate text-caption text-muted-foreground">
            {skillName}
          </span>
        ) : null}
      </div>
      <div className="flex shrink-0 flex-col items-end">
        <span className="font-mono text-body tabular-nums">{amount}</span>
        <span className="font-mono text-caption tabular-nums text-muted-foreground">
          {t(($) => $.billing.balance_after, {
            amount: formatCredits(
              microToCredits(transaction.balanceAfterMicro),
              locale,
            ),
          })}
        </span>
      </div>
    </li>
  );
}

function BillingSkeleton() {
  return (
    <div className="flex flex-col gap-3">
      <Skeleton className="h-24 w-full rounded-lg" />
      <Skeleton className="h-5 w-20" />
      <Skeleton className="h-24 w-full rounded-lg" />
    </div>
  );
}
