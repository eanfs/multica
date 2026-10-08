"use client";

import { useCallback, useEffect, useState } from "react";
import { CircleAlert, Gauge } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
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
  offline: { dot: "bg-warning", tone: "text-warning" },
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
  // Ids whose own detail read has reported a terminal status. The list read is
  // a one-shot snapshot — it has no interval and the client disables
  // refetch-on-focus — so its stored status can stay "queued" long after the
  // generation settles. The row's polled detail is the live source, and it is
  // what removes the row rather than only flipping its badge.
  const [settled, setSettled] = useState<ReadonlySet<string>>(() => new Set());

  const target = runtime.data?.value;
  // A row leaves the running list when either read says it is terminal: the
  // list already filters the statuses it knows, and a row whose detail settles
  // is dropped even before the list catches up.
  const active = (generations.data?.value ?? []).filter(
    (generation) =>
      !isAuroraGenerationTerminal(generation.status) &&
      !settled.has(generation.id),
  );

  const markSettled = useCallback((id: string) => {
    setSettled((previous) => {
      if (previous.has(id)) return previous;
      const next = new Set(previous);
      next.add(id);
      return next;
    });
  }, []);

  return (
    <div className="flex h-full min-h-0 flex-col">
      <CollectionPageHeader icon={Gauge} title={t(($) => $.runtime.title)} />

      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
        <div className="flex flex-col gap-6">
          {runtime.isPending ? (
            <Skeleton className="h-16 w-full rounded-lg" />
          ) : (
            <RuntimeNodeCard
              target={target}
              onRetry={() => void runtime.refetch()}
              retrying={runtime.isFetching}
            />
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
                    onSettled={markSettled}
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
  onRetry,
  retrying,
}: {
  target: AuroraExecutionTarget | undefined;
  onRetry: () => void;
  retrying: boolean;
}) {
  const { t } = useT("aurora");
  const state = target?.state ?? "unconfigured";
  const visual = STATE_VISUAL[state];
  // Recovery only re-reads the projection; it never provisions or restarts a node.
  const recovery =
    state === "failed" || state === "unconfigured" || state === "offline";
  const code = target?.node?.errorCode;
  const reason =
    code ?? (state === "unconfigured" ? "runtime_unconfigured" : "runtime_offline");

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
          {t(($) => $.runtime.reasons[reason])}
        </p>
      ) : null}
      {recovery ? (
        <Button
          variant="outline"
          size="sm"
          className="self-start"
          onClick={onRetry}
          disabled={retrying}
          aria-busy={retrying}
        >
          {t(($) => $.runtime.retry)}
        </Button>
      ) : null}
    </section>
  );
}

/**
 * One generation that has not settled yet.
 *
 * Its own detail query is the polling source: it reuses the existing 3s
 * cadence and stops itself once the server reports a terminal status, so the
 * row is live without a second status channel. Reporting that terminal status
 * up is also what removes the row — the list snapshot cannot be trusted to
 * catch up on its own.
 */
function ActiveGenerationRow({
  generation,
  onSettled,
}: {
  generation: AuroraGeneration;
  onSettled: (id: string) => void;
}) {
  const { t } = useT("aurora");
  const detail = useAuroraGenerationDetail(generation.id);
  const current = detail.data?.value ?? generation;
  const status = generationStatusLabel(current.status);

  useEffect(() => {
    if (isAuroraGenerationTerminal(current.status)) {
      onSettled(generation.id);
    }
  }, [current.status, generation.id, onSettled]);

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
