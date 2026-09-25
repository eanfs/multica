"use client";

import { useMemo, useState } from "react";
import { CircleAlert, Download, Library } from "lucide-react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Alert, AlertDescription } from "@multica/ui/components/ui/alert";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { Spinner } from "@multica/ui/components/ui/spinner";
import {
  auroraAssetDownloadPath,
  isAuroraDegraded,
  isAuroraGenerationTerminal,
  useAuroraAssets,
  useAuroraGenerations,
  useAuroraSkills,
  useDeleteAuroraAsset,
  type AuroraAsset,
  type AuroraGeneration,
} from "@multica/core/aurora";
import {
  CollectionPageHeader,
  CollectionPageState,
} from "../layout/collection-page";
import { useLocale, useT } from "../i18n";
import { formatMicroCredits } from "./format";
import { generationStatusLabel, skillDisplayNamesById } from "./labels";
import { AuroraLoadFailed } from "./load-failed";

/**
 * The library: what has been generated, and the files those generations
 * produced.
 *
 * The two lists come from their own endpoints rather than one nesting the
 * other — a generation is worth watching while it runs, and its assets only
 * exist once it finishes — so they are rendered side by side instead of being
 * reconciled into a single tree the server does not offer.
 */
export function WorksList() {
  const { t } = useT("aurora");
  const locale = useLocale();
  const generationsQuery = useAuroraGenerations();
  const assetsQuery = useAuroraAssets();
  const skillsQuery = useAuroraSkills();
  const deleteAsset = useDeleteAuroraAsset();
  const [pendingDelete, setPendingDelete] = useState<AuroraAsset | null>(null);
  const [deleteFailed, setDeleteFailed] = useState(false);

  const generations = generationsQuery.data?.value ?? [];
  const assets = assetsQuery.data?.value ?? [];
  const skills = skillsQuery.data?.value;

  // The catalog is the only place a generation's `skillId` becomes a name. It
  // is the same query the directory reads, so it is already in the cache when
  // the user arrives here.
  const skillNames = useMemo(
    () => skillDisplayNamesById(skills ?? [], locale),
    [skills, locale],
  );

  // Whether an unresolved `skillId` means "the catalog dropped this entry" or
  // "the catalog never loaded". Without the distinction a failed catalog request
  // stamped "Unknown skill" on every row — a screenful of assertions the client
  // had no basis for. A degraded catalog is the same case: it parses to an empty
  // list, which resolves the query without ever having read the catalog. Billing
  // resolves names the same way and stays silent instead, which is what a row
  // with nothing to say should do.
  const catalogLoaded =
    skills !== undefined && !isAuroraDegraded(skillsQuery.data);

  async function confirmDelete() {
    const asset = pendingDelete;
    if (!asset) return;
    setDeleteFailed(false);
    try {
      await deleteAsset.mutateAsync(asset.id);
    } catch {
      // The row stays: the server also drops the stored object, so an
      // optimistic removal would leave the list claiming a file is gone when
      // the delete never happened.
      setDeleteFailed(true);
    }
    // Closed on both paths. `AlertDialogAction` is a plain Button here, not a
    // close primitive, so leaving it open on failure parked the user in front
    // of a modal — which is also what hid the failure: the report renders
    // behind the overlay, where it is both covered and `aria-hidden`, so a
    // sighted user saw a dialog that did nothing and a screen-reader user
    // heard nothing at all.
    setPendingDelete(null);
  }

  const isLoading = generationsQuery.isPending || assetsQuery.isPending;
  // A read the schema rejected is a load failure even though the query
  // resolved: it left `[]` behind, and "nothing here" is a claim the client
  // never read. A *failed* read is only fatal when it left nothing behind — a
  // background refetch that dropped over rows already in cache is a stale list,
  // and replacing a readable one with an error card is the worse trade.
  const loadFailed =
    ((generationsQuery.isError || isAuroraDegraded(generationsQuery.data)) &&
      generations.length === 0) ||
    ((assetsQuery.isError || isAuroraDegraded(assetsQuery.data)) &&
      assets.length === 0);

  return (
    <div className="flex h-full min-h-0 flex-col">
      <CollectionPageHeader
        icon={Library}
        title={t(($) => $.works.title)}
      />

      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
        {isLoading ? (
          <WorksSkeleton />
        ) : loadFailed ? (
          <AuroraLoadFailed
            title={t(($) => $.works.load_failed_title)}
            onRetry={() => {
              void generationsQuery.refetch();
              void assetsQuery.refetch();
            }}
          />
        ) : (
          <div className="flex flex-col gap-6">
            {deleteFailed ? (
              <Alert variant="destructive">
                <CircleAlert aria-hidden="true" />
                <AlertDescription>
                  {t(($) => $.works.delete_failed)}
                </AlertDescription>
              </Alert>
            ) : null}

            <section className="flex flex-col gap-2">
              <h2 className="text-label font-medium">
                {t(($) => $.works.generations_title)}
              </h2>
              {generations.length === 0 ? (
                <CollectionPageState
                  icon={Library}
                  title={t(($) => $.works.generations_empty_title)}
                  description={t(($) => $.works.generations_empty_description)}
                />
              ) : (
                <ul className="rounded-lg border border-surface-border">
                  {generations.map((generation) => (
                    <GenerationRow
                      key={generation.id}
                      generation={generation}
                      skillName={
                        skillNames.get(generation.skillId) ??
                        (catalogLoaded
                          ? t(($) => $.works.skill_unknown)
                          : null)
                      }
                      credits={
                        // A failed generation is refunded in full, so the
                        // amount it reserved is not a cost and printing it as
                        // one claims a spend the client never read. Null is
                        // "nothing was charged", not "we do not know".
                        generationStatusLabel(generation.status) === "failed"
                          ? null
                          : formatMicroCredits(
                              generation.creditsReserved,
                              locale,
                            )
                      }
                      noPrompt={t(($) => $.works.no_prompt)}
                    />
                  ))}
                </ul>
              )}
            </section>

            <section className="flex flex-col gap-2">
              <h2 className="text-label font-medium">
                {t(($) => $.works.assets_title)}
              </h2>
              {assets.length === 0 ? (
                <CollectionPageState
                  icon={Library}
                  title={t(($) => $.works.assets_empty_title)}
                  description={t(($) => $.works.assets_empty_description)}
                />
              ) : (
                <ul className="rounded-lg border border-surface-border">
                  {assets.map((asset) => (
                    <AssetRow
                      key={asset.id}
                      asset={asset}
                      onRequestDelete={() => setPendingDelete(asset)}
                    />
                  ))}
                </ul>
              )}
            </section>
          </div>
        )}
      </div>

      <AlertDialog
        open={pendingDelete !== null}
        onOpenChange={(open) => {
          if (!open) setPendingDelete(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.works.delete_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.works.delete_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.cancel)}</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={deleteAsset.isPending}
              aria-busy={deleteAsset.isPending}
              onClick={() => void confirmDelete()}
            >
              {deleteAsset.isPending ? <Spinner aria-hidden="true" /> : null}
              {t(($) => $.works.delete_confirm)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

/** One generation: what was asked for, by which skill, and where it got to. */
function GenerationRow({
  generation,
  skillName,
  credits,
  noPrompt,
}: {
  generation: AuroraGeneration;
  /** Null when the catalog could not name the skill — nothing to say, not a fact. */
  skillName: string | null;
  /** Null when nothing was charged for it — a failed generation is refunded. */
  credits: string | null;
  noPrompt: string;
}) {
  const { t } = useT("aurora");
  const status = generationStatusLabel(generation.status);
  return (
    <li className="flex items-center gap-3 border-b border-surface-border px-3 py-2 last:border-b-0">
      <div className="flex min-w-0 flex-1 flex-col">
        <span className="truncate text-body">
          {generation.prompt || noPrompt}
        </span>
        {skillName ? (
          <span className="truncate text-caption text-muted-foreground">
            {skillName}
          </span>
        ) : null}
      </div>
      {credits ? (
        <span className="shrink-0 font-mono text-caption tabular-nums text-muted-foreground">
          {t(($) => $.credits, { credits })}
        </span>
      ) : null}
      <Badge
        variant={
          isAuroraGenerationTerminal(generation.status) ? "outline" : "secondary"
        }
        className="shrink-0"
      >
        {t(($) => $.composer.status[status])}
      </Badge>
    </li>
  );
}

/**
 * One stored file.
 *
 * Download is a plain anchor rather than a fetch: the route redirects to a
 * short-lived signed URL, so the browser has to follow it itself (the same
 * shape as an attachment download).
 */
function AssetRow({
  asset,
  onRequestDelete,
}: {
  asset: AuroraAsset;
  onRequestDelete: () => void;
}) {
  const { t } = useT("aurora");
  // A row that carries no format falls back to its kind, and then has nothing
  // else to say — the second line is only there to add something.
  const detail = asset.format ? asset.kind : null;
  return (
    <li className="flex items-center gap-3 border-b border-surface-border px-3 py-2 last:border-b-0">
      <div className="flex min-w-0 flex-1 flex-col">
        <span className="truncate text-body">{asset.format ?? asset.kind}</span>
        {detail ? (
          <span className="truncate text-caption text-muted-foreground">
            {detail}
          </span>
        ) : null}
      </div>
      <a
        href={auroraAssetDownloadPath(asset.id)}
        target="_blank"
        rel="noopener noreferrer"
        className="inline-flex shrink-0 items-center gap-1 text-body text-muted-foreground transition-colors hover:text-foreground focus-visible:text-foreground"
      >
        <Download aria-hidden="true" className="size-3.5" />
        {t(($) => $.works.download)}
      </a>
      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="shrink-0"
        onClick={onRequestDelete}
      >
        {t(($) => $.works.delete)}
      </Button>
    </li>
  );
}

function WorksSkeleton() {
  return (
    <div className="flex flex-col gap-2">
      <Skeleton className="h-5 w-28" />
      <Skeleton className="h-24 w-full rounded-lg" />
      <Skeleton className="h-5 w-24" />
      <Skeleton className="h-24 w-full rounded-lg" />
    </div>
  );
}
