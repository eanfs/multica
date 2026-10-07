"use client";

import { useMemo, useState } from "react";
import { CircleAlert, ExternalLink, FileDown, History } from "lucide-react";
import { Badge } from "@multica/ui/components/ui/badge";
import { buttonVariants } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@multica/ui/components/ui/tabs";
import {
  auroraAssetDownloadPath,
  isAuroraDegraded,
  isAuroraGenerationTerminal,
  useAuroraGenerationDetail,
  useAuroraGenerations,
  useAuroraSkills,
  type AuroraAsset,
  type AuroraGeneration,
  type AuroraSkill,
} from "@multica/core/aurora";
import {
  CollectionPageHeader,
  CollectionPageState,
} from "../layout/collection-page";
import { PAGE_TOOLBAR } from "../layout/page-header";
import { useLocale, useT, useTimeAgo } from "../i18n";
import { formatMicroCredits } from "./format";
import {
  generationStatusLabel,
  skillDisplayName,
  skillDisplayNamesById,
} from "./labels";
import { AuroraLoadFailed } from "./load-failed";
import {
  AURORA_HISTORY_SKILL_ALL,
  AURORA_HISTORY_STATUS_ALL,
  filterAuroraHistory,
  type AuroraHistoryStatusFilter,
} from "./history-filter";

// Stable references for "not loaded yet", so the derived values below do not
// see a fresh array on every render. Same reasoning as SkillDirectory.
const EMPTY_GENERATIONS: AuroraGeneration[] = [];
const EMPTY_SKILLS: AuroraSkill[] = [];
const EMPTY_ASSETS: AuroraAsset[] = [];

// How many thumbnails one row loads before the rest fall back to download rows.
// A generation can produce many files, and an unbounded grid would fetch every
// one of them on first paint. The cap bounds the eager image loads; the overflow
// stays reachable as a labelled download.
const MAX_THUMBNAILS = 4;

/** The asset and the generation it belongs to, held while the dialog is open. */
interface PreviewState {
  asset: AuroraAsset;
  generation: AuroraGeneration;
}

/** Whether an asset has an image to render, narrowing away the nullable URL. */
function isPreviewableImage(
  asset: AuroraAsset,
): asset is AuroraAsset & { mediaUrl: string } {
  return asset.kind === "image" && asset.mediaUrl !== null;
}

/**
 * What a row charges, or null when there is nothing to say.
 *
 * A failed generation is refunded in full, so the amount it reserved is not a
 * cost. A settled generation reports what settlement actually charged — which
 * can be zero, and zero is not a spend. Only a generation still in flight shows
 * its reservation, because that is the only amount that currently exists.
 */
function creditsFor(generation: AuroraGeneration): number | null {
  if (generationStatusLabel(generation.status) === "failed") return null;
  if (isAuroraGenerationTerminal(generation.status)) {
    return generation.creditsCharged > 0 ? generation.creditsCharged : null;
  }
  return generation.creditsReserved > 0 ? generation.creditsReserved : null;
}

/**
 * Aurora's generation history: past generations and the results they produced.
 *
 * The library served the files but not the request that made them, and the
 * composer's Result area only ever showed the one generation it started. This
 * screen pairs each generation summary with its own assets, so a finished
 * result is reachable long after the drawer that started it closed.
 *
 * The list and each row's detail are two reads because that is what the API
 * offers: the list is a page of summaries, and only the detail endpoint returns
 * a generation together with its assets. The detail is preferred for the status
 * too — the list's stored value lags the task-derived one — so a generation
 * that has just settled renders as finished here.
 */
export function HistoryList() {
  const { t } = useT("aurora");
  const locale = useLocale();
  const generationsQuery = useAuroraGenerations();
  const skillsQuery = useAuroraSkills();
  const [status, setStatus] = useState<AuroraHistoryStatusFilter>(
    AURORA_HISTORY_STATUS_ALL,
  );
  const [skillId, setSkillId] = useState<string>(AURORA_HISTORY_SKILL_ALL);
  const [preview, setPreview] = useState<PreviewState | null>(null);

  const generations = generationsQuery.data?.value ?? EMPTY_GENERATIONS;
  const skills = skillsQuery.data?.value ?? EMPTY_SKILLS;

  const skillNames = useMemo(
    () => skillDisplayNamesById(skills, locale),
    [skills, locale],
  );

  // A failed or degraded catalog is not "the catalog dropped this entry":
  // without the distinction a failed request stamps "Unknown skill" on every
  // row, which is a claim the client never read. WorksList resolves names the
  // same way.
  const catalogLoaded =
    skillsQuery.data !== undefined && !isAuroraDegraded(skillsQuery.data);

  const visible = useMemo(
    () => filterAuroraHistory(generations, status, skillId),
    [generations, status, skillId],
  );

  const hasGenerations = generations.length > 0;
  // A read the schema rejected is a load failure even though it resolved: it
  // left an empty list behind, and "nothing here" is a claim the client never
  // read. A failed refetch that kept rows already in cache is a stale list,
  // not an error screen.
  const loadFailed =
    generationsQuery.isError || isAuroraDegraded(generationsQuery.data);

  return (
    <div className="flex h-full min-h-0 flex-col">
      <CollectionPageHeader
        icon={History}
        title={t(($) => $.history.title)}
        count={generations.length}
      />

      <Tabs
        value={status}
        onValueChange={(next) => setStatus(next as AuroraHistoryStatusFilter)}
        className="flex min-h-0 flex-1 flex-col gap-0"
      >
        <div className={PAGE_TOOLBAR}>
          <TabsList>
            <TabsTrigger value={AURORA_HISTORY_STATUS_ALL}>
              {t(($) => $.history.status_all)}
            </TabsTrigger>
            <TabsTrigger value="completed">
              {t(($) => $.composer.status.completed)}
            </TabsTrigger>
            <TabsTrigger value="failed">
              {t(($) => $.composer.status.failed)}
            </TabsTrigger>
          </TabsList>
          {skills.length > 0 ? (
            <Select
              items={[
                {
                  value: AURORA_HISTORY_SKILL_ALL,
                  label: t(($) => $.history.skill_all),
                },
                ...skills.map((skill) => ({
                  value: skill.id,
                  label: skillDisplayName(skill, locale),
                })),
              ]}
              value={skillId}
              onValueChange={(next) =>
                setSkillId(next ?? AURORA_HISTORY_SKILL_ALL)
              }
            >
              <SelectTrigger
                size="sm"
                aria-label={t(($) => $.history.skill_label)}
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent align="end">
                <SelectItem value={AURORA_HISTORY_SKILL_ALL}>
                  {t(($) => $.history.skill_all)}
                </SelectItem>
                {skills.map((skill) => (
                  <SelectItem key={skill.id} value={skill.id}>
                    {skillDisplayName(skill, locale)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : null}
        </div>

        <TabsContent
          value={status}
          className="min-h-0 flex-1 overflow-y-auto px-4 py-3"
        >
          {generationsQuery.isPending ? (
            <HistorySkeleton />
          ) : loadFailed && !hasGenerations ? (
            <AuroraLoadFailed
              title={t(($) => $.history.load_failed_title)}
              onRetry={() => void generationsQuery.refetch()}
            />
          ) : !hasGenerations ? (
            <CollectionPageState
              icon={History}
              title={t(($) => $.history.empty_title)}
              description={t(($) => $.history.empty_description)}
            />
          ) : visible.length === 0 ? (
            <CollectionPageState
              icon={History}
              title={t(($) => $.history.no_match_title)}
              description={t(($) => $.history.no_match_description)}
            />
          ) : (
            <ul className="rounded-lg border border-surface-border">
              {visible.map((generation) => (
                <HistoryRow
                  key={generation.id}
                  generation={generation}
                  skillName={
                    skillNames.get(generation.skillId) ??
                    (catalogLoaded ? t(($) => $.works.skill_unknown) : null)
                  }
                  onPreview={(asset, current) =>
                    setPreview({ asset, generation: current })
                  }
                />
              ))}
            </ul>
          )}
        </TabsContent>
      </Tabs>

      <PreviewDialog
        preview={preview}
        onOpenChange={(open) => {
          if (!open) setPreview(null);
        }}
      />
    </div>
  );
}

/**
 * One generation: what was asked, how it ended, and the files it produced.
 *
 * The row owns its own detail read. A generation's assets only exist on the
 * detail endpoint, and fetching them from the workspace-wide asset list would
 * hit that list's page limit — silently hiding the results of older rows,
 * which is the exact thing this screen exists to fix.
 */
function HistoryRow({
  generation,
  skillName,
  onPreview,
}: {
  generation: AuroraGeneration;
  /** Null when the catalog could not name the skill — nothing to say, not a fact. */
  skillName: string | null;
  onPreview: (asset: AuroraAsset, generation: AuroraGeneration) => void;
}) {
  const { t } = useT("aurora");
  const locale = useLocale();
  const timeAgo = useTimeAgo();
  const detailQuery = useAuroraGenerationDetail(generation.id);

  // Prefer the detail's live status and settlement fields; keep the list's
  // summary until it arrives so the row never renders blank. A detail body the
  // schema rejected resolves to a null value, which falls back the same way.
  const current = detailQuery.data?.value ?? generation;
  const status = generationStatusLabel(current.status);
  const credits = creditsFor(current);
  const assets = detailQuery.data?.value?.assets ?? EMPTY_ASSETS;
  const images = assets.filter(isPreviewableImage);
  const previewable = images.slice(0, MAX_THUMBNAILS);
  const files = [
    ...assets.filter((asset) => !isPreviewableImage(asset)),
    ...images.slice(MAX_THUMBNAILS),
  ];

  return (
    <li className="flex flex-col gap-2 border-b border-surface-border px-3 py-3 last:border-b-0">
      <div className="flex items-start gap-3">
        <div className="flex min-w-0 flex-1 flex-col">
          <span className="truncate text-body">
            {current.prompt || t(($) => $.works.no_prompt)}
          </span>
          <span className="flex min-w-0 items-center gap-2 text-caption text-muted-foreground">
            {skillName ? <span className="truncate">{skillName}</span> : null}
            {current.createdAt ? (
              <time dateTime={current.createdAt} className="shrink-0">
                {timeAgo(current.createdAt)}
              </time>
            ) : null}
          </span>
        </div>
        {credits !== null ? (
          <span className="shrink-0 font-mono text-caption tabular-nums text-muted-foreground">
            {t(($) => $.credits, {
              credits: formatMicroCredits(credits, locale),
            })}
          </span>
        ) : null}
        <Badge
          variant={
            isAuroraGenerationTerminal(current.status) ? "outline" : "secondary"
          }
          className="shrink-0"
        >
          {t(($) => $.composer.status[status])}
        </Badge>
      </div>

      {status === "failed" && current.error ? (
        <p className="flex items-start gap-1.5 text-caption text-destructive">
          <CircleAlert aria-hidden="true" className="mt-0.5 size-3.5 shrink-0" />
          <span className="min-w-0 break-words">{current.error}</span>
        </p>
      ) : null}

      {detailQuery.isPending ? (
        <Skeleton className="h-20 w-20 rounded-md" />
      ) : previewable.length > 0 || files.length > 0 ? (
        <div className="flex flex-col gap-2">
          {previewable.length > 0 ? (
            <ul className="flex flex-wrap gap-2">
              {previewable.map((asset) => (
                <li key={asset.id}>
                  <button
                    type="button"
                    onClick={() => onPreview(asset, current)}
                    aria-label={t(($) => $.history.preview_label, {
                      format: asset.format ?? asset.kind,
                    })}
                    className="group size-20 overflow-hidden rounded-md border border-surface-border bg-muted transition-colors hover:border-foreground/20 focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
                  >
                    {/* A fixed box with width and height keeps a slow-loading
                        image from moving the rows below it. */}
                    <img
                      src={asset.mediaUrl}
                      alt=""
                      loading="lazy"
                      width={80}
                      height={80}
                      className="size-full object-cover transition-transform group-hover:scale-105"
                    />
                  </button>
                </li>
              ))}
            </ul>
          ) : null}
          {files.length > 0 ? (
            <ul className="flex flex-col gap-1">
              {files.map((asset) => (
                <li key={asset.id}>
                  <a
                    href={auroraAssetDownloadPath(asset.id)}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center gap-1.5 text-body text-muted-foreground underline decoration-muted-foreground/30 underline-offset-4 transition-colors hover:text-foreground focus-visible:text-foreground"
                  >
                    <FileDown aria-hidden="true" className="size-3.5 shrink-0" />
                    <span>{asset.format ?? asset.kind}</span>
                    <span>{t(($) => $.works.download)}</span>
                  </a>
                </li>
              ))}
            </ul>
          ) : null}
        </div>
      ) : isAuroraGenerationTerminal(current.status) ? (
        <p className="text-caption text-muted-foreground">
          {t(($) => $.composer.result_empty)}
        </p>
      ) : null}
    </li>
  );
}

/**
 * The larger view a thumbnail opens: the full-size file, a link to its own
 * media URL, and the download route.
 *
 * DialogTitle is required for the dialog's accessible name; the generation's
 * prompt is the most useful one, with the shared empty-prompt copy as fallback.
 */
function PreviewDialog({
  preview,
  onOpenChange,
}: {
  preview: PreviewState | null;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useT("aurora");
  return (
    <Dialog open={preview !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>
            {preview?.generation.prompt || t(($) => $.works.no_prompt)}
          </DialogTitle>
          <DialogDescription>
            {t(($) => $.history.preview_description)}
          </DialogDescription>
        </DialogHeader>
        {preview?.asset.mediaUrl ? (
          <img
            src={preview.asset.mediaUrl}
            alt=""
            className="max-h-[70vh] w-full rounded-md object-contain"
          />
        ) : null}
        <DialogFooter>
          {preview?.asset.mediaUrl ? (
            <a
              href={preview.asset.mediaUrl}
              target="_blank"
              rel="noopener noreferrer"
              className={buttonVariants({ variant: "outline", size: "sm" })}
            >
              <ExternalLink aria-hidden="true" className="size-3.5" />
              {t(($) => $.history.open_in_new_tab)}
            </a>
          ) : null}
          {preview ? (
            <a
              href={auroraAssetDownloadPath(preview.asset.id)}
              target="_blank"
              rel="noopener noreferrer"
              className={buttonVariants({ variant: "default", size: "sm" })}
            >
              <FileDown aria-hidden="true" className="size-3.5" />
              {t(($) => $.works.download)}
            </a>
          ) : null}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** Placeholder rows at the list's own geometry, so the page does not jump. */
function HistorySkeleton() {
  return (
    <div className="flex flex-col gap-2">
      {Array.from({ length: 3 }, (_, index) => (
        <Skeleton key={index} className="h-24 w-full rounded-lg" />
      ))}
    </div>
  );
}
