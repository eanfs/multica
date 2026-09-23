"use client";

import { useState, type FormEvent } from "react";
import { CircleAlert, CreditCard, Sparkles } from "lucide-react";
import {
  Alert,
  AlertAction,
  AlertDescription,
  AlertTitle,
} from "@multica/ui/components/ui/alert";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Label } from "@multica/ui/components/ui/label";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@multica/ui/components/ui/sheet";
import { Spinner } from "@multica/ui/components/ui/spinner";
import { Textarea } from "@multica/ui/components/ui/textarea";
import {
  auroraAssetDownloadPath,
  isAuroraGenerationTerminal,
  isAuroraInsufficientCreditsError,
  isAuroraRateLimitError,
  useAuroraBalance,
  useAuroraGenerationDetail,
  useCreateAuroraGeneration,
  type AuroraSkill,
} from "@multica/core/aurora";
import { AppLink } from "../navigation";
import { useLocale, useT } from "../i18n";
import { formatCredits, formatMicroCredits } from "./format";
import { generationStatusLabel, skillDisplayName } from "./labels";

/**
 * The task drawer: turn one skill into one generation and follow it to its
 * result.
 *
 * The drawer is opened from the skill grid rather than routed to, so it takes
 * the skill it runs as a prop and reports dismissal back up — the grid owns
 * which skill is selected, exactly as the platform owns the URL. `worksHref`
 * is supplied by the host app for the same reason `paths` is not imported here:
 * the library route belongs to the app that defines it.
 */
export interface GenerationComposerProps {
  /** The skill to run, or null while no drawer is open. */
  skill: AuroraSkill | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The app's library route. Omitted, the result offers no link. */
  worksHref?: string;
  /** The app's checkout route. Omitted, the shortfall offers no way to top up. */
  topUpHref?: string;
}

export function GenerationComposer({
  skill,
  open,
  onOpenChange,
  worksHref,
  topUpHref,
}: GenerationComposerProps) {
  if (!skill) return null;
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-full gap-0 sm:max-w-md">
        {/* Keyed on the skill so switching skills remounts the body: the draft,
            the in-flight generation and any failure all belong to the skill
            that was open, and none of them should survive into another's. */}
        <ComposerBody
          key={skill.id}
          skill={skill}
          worksHref={worksHref}
          topUpHref={topUpHref}
        />
      </SheetContent>
    </Sheet>
  );
}

/**
 * What became of the one submit this drawer allows.
 *
 * Modelled as a state rather than three loose values so "no generation yet" and
 * "the create call failed" cannot both be true — the status badge and the error
 * alert are two renderings of the same question.
 */
type SubmitOutcome =
  | { kind: "idle" }
  | { kind: "started"; generationId: string }
  | { kind: "failed"; error: unknown }
  /**
   * The create call answered 2xx with a body that could not be read, so the
   * server may well have enqueued the generation and reserved its credits. It
   * is not a failure — there is simply no id to follow — which is why it is not
   * the generic `failed`: telling the user to retry here is how a double charge
   * happens. The copy sends them to the library instead, and the submit button
   * is spent with it, so the drawer cannot say one thing and do another.
   */
  | { kind: "unreadable" };

function ComposerBody({
  skill,
  worksHref,
  topUpHref,
}: {
  skill: AuroraSkill;
  worksHref?: string;
  topUpHref?: string;
}) {
  const { t } = useT("aurora");
  const locale = useLocale();
  const create = useCreateAuroraGeneration();
  const [prompt, setPrompt] = useState("");
  const [outcome, setOutcome] = useState<SubmitOutcome>({ kind: "idle" });

  const generationId = outcome.kind === "started" ? outcome.generationId : "";
  const detail = useAuroraGenerationDetail(generationId);
  const generation = detail.data;

  const trimmedPrompt = prompt.trim();
  // One submission per drawer. A second POST reserves the skill's credits a
  // second time, and the drawer is only showing one generation's progress — so
  // the button is spent once the server has accepted the first one. A *failed*
  // submit stays live, because retrying that costs nothing; an *unreadable* one
  // does not, because the server may have accepted it (see SubmitOutcome) and
  // the copy sends the user to the library before the next attempt.
  const canSubmit =
    skill.available &&
    trimmedPrompt.length > 0 &&
    !create.isPending &&
    outcome.kind !== "started" &&
    outcome.kind !== "unreadable";

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!canSubmit) return;
    setOutcome({ kind: "idle" });
    try {
      const created = await create.mutateAsync({
        skillId: skill.id,
        prompt: trimmedPrompt,
      });
      setOutcome(
        created
          ? { kind: "started", generationId: created.id }
          : { kind: "unreadable" },
      );
    } catch (error) {
      setOutcome({ kind: "failed", error });
    }
  }

  // Read from the raw status rather than the label: the label has already
  // collapsed an unrecognised status into "unknown", which is not a reason to
  // stop showing that the server is still working.
  const isTerminal = generation
    ? isAuroraGenerationTerminal(generation.status)
    : false;
  const assets = generation?.assets ?? [];
  // A finished generation reports its outcome even when it produced no files —
  // "nothing came back" is a result the user needs to see, not a reason to
  // render nothing.
  const showResult =
    assets.length > 0 ||
    (generation ? generationStatusLabel(generation.status) === "completed" : false);

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
      <SheetHeader className="p-0">
        <SheetTitle>{skillDisplayName(skill, locale)}</SheetTitle>
        <SheetDescription>
          {t(($) => $.composer.cost, {
            credits: formatCredits(skill.credits, locale),
          })}
        </SheetDescription>
      </SheetHeader>

      {!skill.available ? (
        <Alert>
          <CircleAlert aria-hidden="true" />
          <AlertTitle>{t(($) => $.composer.unavailable_title)}</AlertTitle>
          <AlertDescription>
            {t(($) => $.composer.unavailable_description)}
          </AlertDescription>
        </Alert>
      ) : null}

      {outcome.kind === "failed" ? (
        <SubmitFailure
          error={outcome.error}
          skill={skill}
          topUpHref={topUpHref}
        />
      ) : null}

      <form className="flex flex-col gap-2" onSubmit={handleSubmit}>
        <Label htmlFor="aurora-prompt">
          {t(($) => $.composer.prompt_label)}
        </Label>
        <Textarea
          id="aurora-prompt"
          value={prompt}
          onChange={(event) => setPrompt(event.target.value)}
          placeholder={t(($) => $.composer.prompt_placeholder)}
          rows={5}
          // The prompt is the whole input, and losing it costs the user their
          // work — so it is kept rather than cleared when the submit fails.
          disabled={!skill.available}
        />
        <Button type="submit" disabled={!canSubmit} aria-busy={create.isPending}>
          {create.isPending ? <Spinner aria-hidden="true" /> : null}
          {create.isPending
            ? t(($) => $.composer.submitting)
            : t(($) => $.composer.submit)}
        </Button>
      </form>

      {outcome.kind === "unreadable" ? (
        <Alert variant="destructive">
          <CircleAlert aria-hidden="true" />
          <AlertDescription>
            {t(($) => $.composer.create_unreadable)}
          </AlertDescription>
        </Alert>
      ) : null}

      {/* An accepted generation always says something about itself. The detail
          read can be in flight, and it can fail — the poll stops on a failed
          read (../core/aurora/queries.ts) — and in both cases the drawer used to
          render nothing at all, which reads as "the submit did nothing" and
          invites a second one.

          A *failed* read is reported next to the last status rather than
          instead of it: React Query keeps the data a failed refetch could not
          replace, so one dropped poll tick says "here is where it was, and I
          cannot see it now" instead of throwing away a status that was read
          successfully a moment ago. */}
      {outcome.kind === "started" ? (
        <>
          {generation ? (
            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-2">
                <Badge variant="outline" role="status">
                  {t(
                    ($) =>
                      $.composer.status[generationStatusLabel(generation.status)],
                  )}
                </Badge>
                {!isTerminal ? <Spinner aria-hidden="true" /> : null}
              </div>
              {generationStatusLabel(generation.status) === "failed" ? (
                <p className="text-caption text-muted-foreground">
                  {t(($) => $.composer.failed_description)}
                </p>
              ) : null}
              {!isTerminal ? (
                <p className="text-caption text-muted-foreground">
                  {t(($) => $.composer.progress_description)}
                </p>
              ) : null}
            </div>
          ) : detail.isError ? null : (
            <div className="flex items-center gap-2">
              <Spinner aria-hidden="true" />
              <p className="text-caption text-muted-foreground">
                {t(($) => $.composer.progress_description)}
              </p>
            </div>
          )}

          {detail.isError ? (
            <Alert variant="destructive">
              <CircleAlert aria-hidden="true" />
              <AlertDescription>
                {t(($) => $.composer.progress_unreadable)}
              </AlertDescription>
              <AlertAction>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => void detail.refetch()}
                >
                  {t(($) => $.retry)}
                </Button>
              </AlertAction>
            </Alert>
          ) : null}
        </>
      ) : null}

      {showResult ? (
        <div className="flex flex-col gap-2">
          <h3 className="text-label font-medium">
            {t(($) => $.composer.result_title)}
          </h3>
          {assets.length > 0 ? (
            <ul className="flex flex-col gap-1">
              {assets.map((asset) => (
                <li key={asset.id}>
                  <a
                    href={auroraAssetDownloadPath(asset.id)}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-body underline decoration-muted-foreground/30 underline-offset-4 transition-colors hover:text-foreground"
                  >
                    {asset.format ?? asset.kind}
                  </a>
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-caption text-muted-foreground">
              {t(($) => $.composer.result_empty)}
            </p>
          )}
          {worksHref ? (
            <AppLink
              href={worksHref}
              className="inline-flex w-fit items-center gap-1 text-body underline decoration-muted-foreground/30 underline-offset-4 transition-colors hover:text-foreground"
            >
              <Sparkles aria-hidden="true" className="size-3.5" />
              {t(($) => $.composer.open_library)}
            </AppLink>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

/**
 * Why the create call was refused, in the terms the user can act on.
 *
 * The server answers 402 for a wallet that cannot cover the skill and 429 for
 * all three of its gates — the monthly quota, the concurrency cap and the
 * frequency limit. The three 429s differ only in a server message that is not a
 * contract, so they share one line rather than guessing which fired; the 402 is
 * the one worth separating, because it names a number the user can change and
 * the one the drawer can point at the way to change it.
 */
function SubmitFailure({
  error,
  skill,
  topUpHref,
}: {
  error: unknown;
  skill: AuroraSkill;
  topUpHref?: string;
}) {
  const { t } = useT("aurora");
  const locale = useLocale();
  const { data: balance } = useAuroraBalance();

  if (isAuroraInsufficientCreditsError(error)) {
    const cost = formatCredits(skill.credits, locale);
    return (
      <Alert variant="destructive">
        <CircleAlert aria-hidden="true" />
        <AlertTitle>{t(($) => $.composer.insufficient_title)}</AlertTitle>
        <AlertDescription>
          {/* The 402 itself is what makes this prompt true, so the price alone
              is a complete sentence. The balance is added only when there is
              one to show: while the wallet query is pending or failed there is
              no number here, and "you have 0" would be a claim the client never
              read — the user would top up against a figure we invented. */}
          {balance === undefined
            ? t(($) => $.composer.insufficient_cost_only, { cost })
            : t(($) => $.composer.insufficient_description, {
                cost,
                balance: formatMicroCredits(balance.availableMicro, locale),
              })}
        </AlertDescription>
        {/* The one refusal the user can do something about, so it carries the
            way to do it — when the app has a checkout route to offer. */}
        {topUpHref ? (
          <AlertAction>
            <AppLink
              href={topUpHref}
              className="inline-flex items-center gap-1 text-body underline decoration-muted-foreground/30 underline-offset-4 transition-colors hover:text-foreground"
            >
              <CreditCard aria-hidden="true" className="size-3.5" />
              {t(($) => $.billing.top_up)}
            </AppLink>
          </AlertAction>
        ) : null}
      </Alert>
    );
  }

  if (isAuroraRateLimitError(error)) {
    return (
      <Alert variant="destructive">
        <CircleAlert aria-hidden="true" />
        <AlertTitle>{t(($) => $.composer.rate_limited_title)}</AlertTitle>
        <AlertDescription>
          {t(($) => $.composer.rate_limited_description)}
        </AlertDescription>
      </Alert>
    );
  }

  return (
    <Alert variant="destructive">
      <CircleAlert aria-hidden="true" />
      <AlertDescription>
        {t(($) => $.composer.create_failed)}
      </AlertDescription>
    </Alert>
  );
}
