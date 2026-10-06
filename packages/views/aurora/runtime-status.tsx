"use client";

import { CircleAlert, Gauge } from "lucide-react";
import { Badge } from "@multica/ui/components/ui/badge";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import {
  isAuroraGenerationTerminal,
  useAuroraGenerationDetail,
  useAuroraGenerations,
  useAuroraRuntime,
  type AuroraExecutionTarget,
  type AuroraGeneration,
  type AuroraRuntimeState,
} from "@multica/core/aurora";
import { CollectionPageHeader } from "../layout/collection-page";
import { useT } from "../i18n";
import { generationStatusLabel } from "./labels";

/**
 * The workspace's execution view.
 *
 * It surfaces the managed sandbox node the Fleet runs this workspace's
 * generations on, plus the generations currently in flight. The node is read
 * from `GET /api/aurora/runtime`; the in-flight rows reuse the existing 3s
 * detail poll rather than a second status protocol, so nothing here opens a
 * realtime channel.
 *
 * Dangerous lifecycle actions stay in the runtime management surface; this
 * screen only reports and guides.
 */

// The same visual vocabulary the runtime manager uses for health
// (packages/views/runtimes/components/shared.tsx): a semantic dot and a shared
// token tone, so a node reads the same here as it does there.
const STATE_VISUAL: Record<
  AuroraRuntimeState,
  { dot: string; tone: string }
> = {
  online: { dot: "bg-success", tone: "text-success" },
  provisioning: { dot: "bg-info", tone: "text-info" },
  failed: { dot: "bg-destructive", tone: "text-destructive" },
  unconfigured: {
    dot: "bg-muted-foreground/40",
    tone: "text-muted-foreground",
  },
};

export function RuntimeStatus() {
  const { t } = useT("aurora");
  const runtime = useAuroraRuntime();
  const generations = useAuroraGenerations();

  const target = runtime.data?.value;
  // The list read carries the stored status, which stays "queued" until the
  // execution layer writes the terminal one back. Only terminal rows are
  // dropped; the row's own detail read keeps the label live.
  const active = (generations.data?.value ?? []).filter(
    (generation) => !isAuroraGenerationTerminal(generation.status),
  );

  return (
    <div className="flex h-full min-h-0 flex-col">
      <CollectionPageHeader icon={Gauge} title={t(($) => $.runtime.title)} />

      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
        <div className="flex flex-col gap-6">
          {runtime.isPending ? (
            <Skeleton className="h-16 w-full rounded-lg" />
          ) : (
            <RuntimeNodeCard target={target} />
          )}

          <section className="flex flex-col gap-2">
            <h2 className="text-label font-medium">
              {t(($) => $.runtime.generations_title)}
            </h2>
            {active.length === 0 ? (
              <p className="text-body text-muted-foreground">
                {t(($) => $.runtime.generations_empty)}
              </p>
            ) : (
              <ul className="rounded-lg border border-surface-border">
                {active.map((generation) => (
                  <ActiveGenerationRow
                    key={generation.id}
                    generation={generation}
                  />
                ))}
              </ul>
            )}
          </section>
        </div>
      </div>
    </div>
  );
}

/**
 * The node's state, with recovery guidance where the state is actionable.
 *
 * `target` is undefined only in the moment between the query erroring and the
 * screen re-rendering; the parser already turns a malformed body into the
 * `unconfigured` projection, so the fallback here says the same thing.
 */
function RuntimeNodeCard({
  target,
}: {
  target: AuroraExecutionTarget | undefined;
}) {
  const { t } = useT("aurora");
  const state = target?.state ?? "unconfigured";
  const visual = STATE_VISUAL[state];
  // Narrowed to the two states whose copy points at how to recover, not merely
  // at what is wrong.
  const recovery: "failed" | "unconfigured" | null =
    state === "failed" || state === "unconfigured" ? state : null;

  return (
    <section className="flex flex-col gap-2 rounded-lg border border-surface-border p-3">
      <div className="flex items-center gap-2">
        {state === "failed" ? (
          <CircleAlert
            aria-hidden="true"
            className={`size-3.5 shrink-0 ${visual.tone}`}
          />
        ) : (
          <span
            aria-hidden="true"
            className={`size-2 shrink-0 rounded-full ${visual.dot}`}
          />
        )}
        <span className={`text-body font-medium ${visual.tone}`}>
          {t(($) => $.runtime.status[state])}
        </span>
      </div>
      {recovery ? (
        <p role="alert" className="text-caption text-warning">
          {t(($) => $.runtime.recovery[recovery])}
        </p>
      ) : null}
    </section>
  );
}

/**
 * One generation that has not settled yet.
 *
 * Its own detail query is the polling source: it reuses the existing 3s
 * cadence and stops itself once the server reports a terminal status, so the
 * row is live without a second status channel.
 */
function ActiveGenerationRow({
  generation,
}: {
  generation: AuroraGeneration;
}) {
  const { t } = useT("aurora");
  const detail = useAuroraGenerationDetail(generation.id);
  const current = detail.data?.value ?? generation;
  const status = generationStatusLabel(current.status);

  return (
    <li className="flex items-center gap-3 border-b border-surface-border px-3 py-2 last:border-b-0">
      <span className="min-w-0 flex-1 truncate text-body">
        {current.prompt || t(($) => $.works.no_prompt)}
      </span>
      <Badge variant="secondary" className="shrink-0">
        {t(($) => $.composer.status[status])}
      </Badge>
    </li>
  );
}
