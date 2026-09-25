"use client";

import { useMemo, useState } from "react";
import { Sparkles } from "lucide-react";
import { Badge } from "@multica/ui/components/ui/badge";
import { Input } from "@multica/ui/components/ui/input";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@multica/ui/components/ui/tabs";
import { cn } from "@multica/ui/lib/utils";
import {
  isAuroraDegraded,
  useAuroraSkills,
  type AuroraSkill,
} from "@multica/core/aurora";
import {
  CollectionPageHeader,
  CollectionPageState,
} from "../layout/collection-page";
import { PAGE_TOOLBAR } from "../layout/page-header";
import { useLocale, useT } from "../i18n";
import { GenerationComposer } from "./generation-composer";
import { formatCredits } from "./format";
import { auroraCategoryLabel, skillDisplayName } from "./labels";
import { AuroraLoadFailed } from "./load-failed";
import { AURORA_CATEGORY_ALL, filterAuroraSkills } from "./skill-filter";

/**
 * The skill directory: the whole catalog, filtered, and the drawer that runs
 * one entry.
 *
 * The directory owns which skill the drawer is showing. Putting that in the URL
 * would make a half-written prompt survive a refresh as a linkable address,
 * which is not what a consumer picking a skill out of a grid expects — so it
 * stays component state, and picking a skill is a click rather than a
 * navigation. `worksHref` and `topUpHref` are forwarded to the drawer so the
 * app keeps owning the library and checkout routes.
 */
export interface SkillDirectoryProps {
  /** The app's library route, offered on a finished generation. */
  worksHref?: string;
  /** The app's checkout route, offered when a skill costs more than the wallet holds. */
  topUpHref?: string;
}

// The grid's own geometry, shared by the cards and the skeleton so the page
// does not jump when the catalog resolves.
const GRID_CLASS = "grid grid-cols-[repeat(auto-fill,minmax(11rem,1fr))] gap-3";

// A stable reference for "the catalog has not loaded yet". `data ?? []` would
// hand the memos below a fresh array on every render while the query is
// pending, which defeats their dependency check and rebuilds the category tabs
// each time. Same reasoning as the EMPTY_* constants in
// `dashboard/components/dashboard-page.tsx`.
const EMPTY_SKILLS: AuroraSkill[] = [];

export function SkillDirectory({
  worksHref,
  topUpHref,
}: SkillDirectoryProps = {}) {
  const { t } = useT("aurora");
  const locale = useLocale();
  const skillsQuery = useAuroraSkills();
  const [query, setQuery] = useState("");
  const [category, setCategory] = useState<string>(AURORA_CATEGORY_ALL);
  const [selected, setSelected] = useState<AuroraSkill | null>(null);

  const skills = skillsQuery.data?.value ?? EMPTY_SKILLS;

  // One tab per category the catalog actually uses, in catalog order, rather
  // than a fixed list: a category a newer server adds gets a tab without a
  // client change, and one it drops stops being offered.
  const categories = useMemo(
    () => Array.from(new Set(skills.map((skill) => skill.category))),
    [skills],
  );

  const visible = useMemo(
    () => filterAuroraSkills(skills, query, category),
    [skills, query, category],
  );

  const hasCatalog = skills.length > 0;
  // A body the schema rejected is not an empty catalog. It parses to `[]` and
  // resolves the query, so `isError` alone would render the empty state — "the
  // directory is empty" is a claim the client never read. Both outcomes take
  // the failure branch; only the one that left rows behind keeps them.
  const catalogUnreadable =
    skillsQuery.isError || isAuroraDegraded(skillsQuery.data);

  return (
    <div className="flex h-full min-h-0 flex-col">
      <CollectionPageHeader
        icon={Sparkles}
        title={t(($) => $.directory.title)}
        count={skills.length}
        actions={
          <Input
            type="search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder={t(($) => $.directory.search_placeholder)}
            aria-label={t(($) => $.directory.search_placeholder)}
            className="w-48 sm:w-56"
          />
        }
      />

      <Tabs
        value={category}
        onValueChange={setCategory}
        className="flex min-h-0 flex-1 flex-col gap-0"
      >
        <div className={PAGE_TOOLBAR}>
          <TabsList>
            <TabsTrigger value={AURORA_CATEGORY_ALL}>
              {t(($) => $.directory.categories.all)}
            </TabsTrigger>
            {categories.map((value) => (
              <TabsTrigger key={value} value={value}>
                {t(($) => $.directory.categories[auroraCategoryLabel(value)])}
              </TabsTrigger>
            ))}
          </TabsList>
        </div>

        <TabsContent
          value={category}
          className="min-h-0 flex-1 overflow-y-auto px-4 py-3"
        >
          {skillsQuery.isPending ? (
            <DirectorySkeleton />
          ) : catalogUnreadable && !hasCatalog ? (
            <AuroraLoadFailed
              title={t(($) => $.directory.load_failed_title)}
              onRetry={() => void skillsQuery.refetch()}
            />
          ) : visible.length === 0 ? (
            <CollectionPageState
              icon={Sparkles}
              title={t(($) => $.directory.empty_title)}
              description={t(($) => $.directory.empty_description)}
            />
          ) : (
            <ul className={GRID_CLASS}>
              {visible.map((skill) => (
                <li key={skill.id}>
                  <SkillCard
                    skill={skill}
                    displayName={skillDisplayName(skill, locale)}
                    credits={formatCredits(skill.credits, locale)}
                    onSelect={() => setSelected(skill)}
                  />
                </li>
              ))}
            </ul>
          )}
        </TabsContent>
      </Tabs>

      <GenerationComposer
        skill={selected}
        open={selected !== null}
        onOpenChange={(open) => {
          if (!open) setSelected(null);
        }}
        worksHref={worksHref}
        topUpHref={topUpHref}
      />
    </div>
  );
}

/**
 * One catalog entry.
 *
 * An unavailable skill is still a live button. It is not a disabled action —
 * the action is "open this skill", and opening it is the only way to read why
 * it cannot run yet; the badge and the muted fill carry the state instead. The
 * price is shown next to it either way, because a roadmap entry is worth
 * pricing.
 */
function SkillCard({
  skill,
  displayName,
  credits,
  onSelect,
}: {
  skill: AuroraSkill;
  displayName: string;
  credits: string;
  onSelect: () => void;
}) {
  const { t } = useT("aurora");
  return (
    <button
      type="button"
      onClick={onSelect}
      className={cn(
        "flex h-full w-full flex-col items-start gap-2 rounded-lg border border-surface-border bg-card p-3 text-left transition-colors",
        "hover:border-foreground/20 hover:bg-muted/50",
        "focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none",
        !skill.available && "text-muted-foreground",
      )}
    >
      <span className="flex w-full items-start justify-between gap-2">
        <span className="text-body font-medium">{displayName}</span>
        {!skill.available ? (
          <Badge variant="outline" className="shrink-0">
            {t(($) => $.directory.unavailable)}
          </Badge>
        ) : null}
      </span>
      <span className="text-caption text-muted-foreground">
        {t(($) => $.credits, { credits })}
      </span>
    </button>
  );
}

/** Placeholder cards at the grid's own geometry, so the page does not jump. */
function DirectorySkeleton() {
  return (
    <ul className={GRID_CLASS}>
      {Array.from({ length: 8 }, (_, index) => (
        <li key={index}>
          <Skeleton className="h-20 w-full rounded-lg" />
        </li>
      ))}
    </ul>
  );
}
