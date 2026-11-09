"use client";

import { useId, useMemo, useState } from "react";
import type { FormEvent, HTMLAttributes } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Cloud,
  Loader2,
  Play,
  RefreshCw,
  Rocket,
  RotateCcw,
  Square,
  Trash2,
} from "@multica/ui/icons";
import { toast } from "sonner";
import type {
  CloudRuntimeNode,
  CloudRuntimeService,
  CreateCloudRuntimeNodeRequest,
} from "@multica/core/runtimes";
import {
  cloudRuntimeNodeListOptions,
  newCloudRuntimeIdempotencyKey,
  useCreateCloudRuntimeNode,
  useDeleteCloudRuntimeNode,
  useRebootCloudRuntimeNode,
  useStartCloudRuntimeNode,
  useStopCloudRuntimeNode,
} from "@multica/core/runtimes";
import { useWorkspaceId } from "@multica/core/hooks";
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
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import {
  cloudRuntimeNodeFailureReason,
  cloudRuntimeOperations,
  isDockerCloudRuntime,
} from "../utils";

const DEFAULT_DISK_SIZE_GB = 20;

export function CloudRuntimeDialog({
  service,
  onClose,
}: {
  service: CloudRuntimeService;
  onClose: () => void;
}) {
  const { t } = useT("runtimes");
  const wsId = useWorkspaceId();
  const idPrefix = `cloud-runtime-${useId().replace(/:/g, "")}`;
  const formId = `${idPrefix}-form`;
  const [name, setName] = useState("");
  const [specChoice, setSpecChoice] = useState<string | null>(null);
  const [diskSizeGB, setDiskSizeGB] = useState(String(DEFAULT_DISK_SIZE_GB));

  const operations = cloudRuntimeOperations(service);
  const docker = isDockerCloudRuntime(service);
  // The advertised allowlist is the only source of node shapes. No fallback
  // names: a spec the provider never offered is rejected by the backend, so an
  // empty list means there is nothing valid to submit.
  const specOptions = useMemo(
    () => service.specs.map((spec) => spec.id),
    [service.specs],
  );
  const spec =
    specChoice && specOptions.includes(specChoice)
      ? specChoice
      : (specOptions[0] ?? "");
  const canCreate = operations.canCreate && specOptions.length > 0;

  const nodesQuery = useQuery(
    cloudRuntimeNodeListOptions(wsId, { limit: 20, offset: 0 }),
  );
  const createNode = useCreateCloudRuntimeNode(wsId);

  const sortedNodes = useMemo(
    () =>
      (nodesQuery.data ?? []).toSorted(
        (a, b) =>
          new Date(b.created_at).getTime() - new Date(a.created_at).getTime(),
      ),
    [nodesQuery.data],
  );

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!canCreate) return;

    const trimmedName = valueOrUndefined(name);
    const request = docker
      ? dockerCreateRequest(trimmedName, spec)
      : ec2CreateRequest(trimmedName, spec, diskSizeGB);

    if (!request) {
      toast.error(t(($) => $.cloud_runtime.validation.disk_size_invalid));
      return;
    }

    try {
      await createNode.mutateAsync(request);
      toast.success(t(($) => $.cloud_runtime.toast_created));
      setName("");
      setSpecChoice(null);
      setDiskSizeGB(String(DEFAULT_DISK_SIZE_GB));
    } catch (error) {
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.cloud_runtime.toast_create_failed),
      );
    }
  };

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="flex max-h-[88vh] flex-col gap-0 p-0 sm:max-w-3xl">
        <DialogHeader className="border-b px-6 py-5">
          <DialogTitle className="flex items-center gap-2 text-title-sm">
            <Cloud className="h-4 w-4 text-muted-foreground" />
            {t(($) => $.cloud_runtime.title)}
          </DialogTitle>
          <DialogDescription className="text-caption">
            {t(($) => $.cloud_runtime.description)}
          </DialogDescription>
          <div className="flex flex-wrap items-center gap-2 text-caption text-muted-foreground">
            <span>
              {t(($) => $.cloud_runtime.provider)}:{" "}
              <span className="font-mono">{service.provider}</span>
            </span>
            {service.persistentStorage && (
              <Badge variant="secondary">
                {t(($) => $.cloud_runtime.persistent_volume)}
              </Badge>
            )}
          </div>
        </DialogHeader>

        <div className="min-h-0 flex-1 overflow-y-auto px-6 py-5">
          <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_minmax(280px,0.82fr)]">
            <form id={formId} onSubmit={handleSubmit} className="space-y-4">
              <div>
                <h3 className="text-body font-medium">
                  {t(($) => $.cloud_runtime.create_title)}
                </h3>
              </div>

              <div className="grid gap-3 sm:grid-cols-2">
                <LabeledInput
                  id={`${idPrefix}-name`}
                  label={t(($) => $.cloud_runtime.fields.name)}
                  value={name}
                  onChange={setName}
                  placeholder={t(($) => $.cloud_runtime.placeholders.name)}
                />
                {specOptions.length > 0 && (
                  <LabeledInput
                    id={`${idPrefix}-spec`}
                    label={t(($) => $.cloud_runtime.spec)}
                    value={spec}
                    onChange={setSpecChoice}
                    options={specOptions}
                  />
                )}
                {!docker && (
                  <LabeledInput
                    id={`${idPrefix}-disk-size`}
                    label={t(($) => $.cloud_runtime.fields.disk_size)}
                    value={diskSizeGB}
                    onChange={setDiskSizeGB}
                    placeholder={String(DEFAULT_DISK_SIZE_GB)}
                    type="number"
                    inputMode="numeric"
                  />
                )}
              </div>

              {!canCreate && (
                <p className="text-caption text-muted-foreground">
                  {t(($) => $.cloud_runtime.unsupported_operation)}
                </p>
              )}
            </form>

            <section className="min-h-0 rounded-md border bg-muted/20">
              <div className="flex items-center justify-between border-b bg-background px-3 py-2.5">
                <h3 className="text-body font-medium">
                  {t(($) => $.cloud_runtime.nodes_title)}
                </h3>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => void nodesQuery.refetch()}
                  disabled={nodesQuery.isFetching}
                  className="h-7 px-2"
                >
                  <RefreshCw
                    className={cn(
                      "h-3.5 w-3.5",
                      nodesQuery.isFetching && "animate-spin",
                    )}
                  />
                  {t(($) => $.cloud_runtime.refresh)}
                </Button>
              </div>

              {nodesQuery.isLoading ? (
                <div className="flex h-40 items-center justify-center">
                  <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
                </div>
              ) : nodesQuery.isError ? (
                <div className="flex h-40 flex-col items-center justify-center px-5 text-center">
                  <p className="text-body font-medium">
                    {t(($) => $.cloud_runtime.nodes_failed)}
                  </p>
                  <p className="mt-1 text-caption text-muted-foreground">
                    {nodesQuery.error instanceof Error
                      ? nodesQuery.error.message
                      : t(($) => $.cloud_runtime.nodes_failed_hint)}
                  </p>
                </div>
              ) : sortedNodes.length === 0 ? (
                <div className="flex h-40 flex-col items-center justify-center px-5 text-center">
                  <Cloud className="h-7 w-7 text-faint-foreground" />
                  <p className="mt-3 text-body font-medium">
                    {t(($) => $.cloud_runtime.nodes_empty)}
                  </p>
                </div>
              ) : (
                <div className="max-h-[410px] overflow-y-auto p-2">
                  <div className="space-y-2">
                    {sortedNodes.map((node) => (
                      <CloudRuntimeNodeRow
                        key={node.id}
                        node={node}
                        wsId={wsId}
                        service={service}
                      />
                    ))}
                  </div>
                </div>
              )}
            </section>
          </div>
        </div>

        <DialogFooter className="m-0 border-t bg-muted/30 px-6 py-3">
          <Button type="button" variant="outline" size="sm" onClick={onClose}>
            {t(($) => $.cloud_runtime.cancel)}
          </Button>
          <Button
            type="submit"
            size="sm"
            form={formId}
            disabled={createNode.isPending || !canCreate}
          >
            {createNode.isPending ? (
              <Loader2 className="h-3.5 w-3.5 animate-spin" />
            ) : (
              <Rocket className="h-3.5 w-3.5" />
            )}
            {createNode.isPending
              ? t(($) => $.cloud_runtime.creating)
              : t(($) => $.cloud_runtime.create)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function LabeledInput({
  id,
  label,
  value,
  onChange,
  placeholder,
  required,
  type = "text",
  inputMode,
  options,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  required?: boolean;
  type?: string;
  inputMode?: HTMLAttributes<HTMLInputElement>["inputMode"];
  options?: readonly string[];
}) {
  if (options) {
    return (
      <div className="space-y-1.5">
        <Label htmlFor={id} className="text-caption text-muted-foreground">
          {label}
        </Label>
        <Select
          items={options.map((option) => ({ value: option, label: option }))}
          value={value}
          onValueChange={(next) => onChange(next ?? value)}
        >
          <SelectTrigger id={id} className="h-9 w-full rounded-md text-body">
            <SelectValue>
              {() => <span className="truncate">{value}</span>}
            </SelectValue>
          </SelectTrigger>
          <SelectContent align="start">
            {options.map((option) => (
              <SelectItem key={option} value={option}>
                {option}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    );
  }

  return (
    <div className="space-y-1.5">
      <Label htmlFor={id} className="text-caption text-muted-foreground">
        {label}
      </Label>
      <Input
        id={id}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        required={required}
        type={type}
        inputMode={inputMode}
        className="h-9 text-body"
      />
    </div>
  );
}

function CloudRuntimeNodeRow({
  node,
  wsId,
  service,
}: {
  node: CloudRuntimeNode;
  wsId: string;
  service: CloudRuntimeService;
}) {
  const { t } = useT("runtimes");
  const operations = cloudRuntimeOperations(service);
  const deleteNode = useDeleteCloudRuntimeNode(wsId);
  const startNode = useStartCloudRuntimeNode(wsId);
  const stopNode = useStopCloudRuntimeNode(wsId);
  const rebootNode = useRebootCloudRuntimeNode(wsId);
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const busy =
    deleteNode.isPending ||
    startNode.isPending ||
    stopNode.isPending ||
    rebootNode.isPending;
  const failureReason = cloudRuntimeNodeFailureReason(node);
  const title =
    node.name.trim() ||
    node.instance_id.trim() ||
    t(($) => $.cloud_runtime.node_fallback_name);
  const created = formatDateTime(node.created_at);

  const reportOperationError = (error: unknown) =>
    toast.error(
      error instanceof Error
        ? error.message
        : t(($) => $.cloud_runtime.toast_operation_failed),
    );

  return (
    <div className="rounded-md border bg-background px-3 py-2.5">
      <div className="flex min-w-0 items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex min-w-0 items-center gap-2">
            <span className="truncate text-body font-medium">{title}</span>
            <CloudRuntimeStatusBadge status={node.status} />
          </div>
          <div className="mt-1 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-caption text-muted-foreground">
            <span>{node.instance_type}</span>
            {node.region && (
              <>
                <span className="text-faint-foreground">/</span>
                <span>{node.region}</span>
              </>
            )}
            {created && (
              <>
                <span className="text-faint-foreground">/</span>
                <span>{created}</span>
              </>
            )}
          </div>
          {failureReason && (
            <p className="mt-1 text-caption text-destructive" role="alert">
              {failureReason}
            </p>
          )}
        </div>
        <div
          role="group"
          aria-label={t(($) => $.cloud_runtime.operations)}
          className="flex shrink-0 items-center gap-1"
        >
          {operations.canStart && (
            <Button
              type="button"
              variant="ghost"
              size="icon-sm"
              aria-label={
                startNode.isPending
                  ? t(($) => $.cloud_runtime.starting)
                  : t(($) => $.cloud_runtime.start)
              }
              title={t(($) => $.cloud_runtime.start)}
              disabled={busy}
              aria-busy={startNode.isPending}
              onClick={() =>
                startNode.mutate(node.instance_id, {
                  onError: reportOperationError,
                })
              }
            >
              {startNode.isPending ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : (
                <Play className="h-3.5 w-3.5" />
              )}
            </Button>
          )}
          {operations.canStop && (
            <Button
              type="button"
              variant="ghost"
              size="icon-sm"
              aria-label={
                stopNode.isPending
                  ? t(($) => $.cloud_runtime.stopping)
                  : t(($) => $.cloud_runtime.stop)
              }
              title={t(($) => $.cloud_runtime.stop)}
              disabled={busy}
              aria-busy={stopNode.isPending}
              onClick={() =>
                stopNode.mutate(node.instance_id, {
                  onError: reportOperationError,
                })
              }
            >
              {stopNode.isPending ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : (
                <Square className="h-3.5 w-3.5" />
              )}
            </Button>
          )}
          {operations.canReboot && (
            <Button
              type="button"
              variant="ghost"
              size="icon-sm"
              aria-label={
                rebootNode.isPending
                  ? t(($) => $.cloud_runtime.rebooting)
                  : t(($) => $.cloud_runtime.reboot)
              }
              title={t(($) => $.cloud_runtime.reboot)}
              disabled={busy}
              aria-busy={rebootNode.isPending}
              onClick={() =>
                rebootNode.mutate(node.instance_id, {
                  onError: reportOperationError,
                })
              }
            >
              {rebootNode.isPending ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : (
                <RotateCcw className="h-3.5 w-3.5" />
              )}
            </Button>
          )}
          {operations.canDelete && (
            <Button
              type="button"
              variant="ghost"
              size="icon-sm"
              className="text-muted-foreground hover:text-destructive"
              disabled={busy}
              onClick={() => setConfirmingDelete(true)}
              aria-label={t(($) => $.cloud_runtime.delete)}
              title={t(($) => $.cloud_runtime.delete)}
            >
              <Trash2 className="h-3.5 w-3.5" />
            </Button>
          )}
        </div>
      </div>
      {node.instance_id && (
        <div className="mt-2 truncate font-mono text-micro text-muted-foreground">
          {node.instance_id}
        </div>
      )}

      <AlertDialog open={confirmingDelete} onOpenChange={setConfirmingDelete}>
        <AlertDialogContent size="sm">
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.cloud_runtime.delete)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.cloud_runtime.delete_irreversible)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleteNode.isPending}>
              {t(($) => $.cloud_runtime.cancel)}
            </AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={deleteNode.isPending}
              aria-busy={deleteNode.isPending}
              onClick={() =>
                deleteNode.mutate(node.instance_id, {
                  onSuccess: () => {
                    setConfirmingDelete(false);
                    toast.success(t(($) => $.cloud_runtime.toast_deleted));
                  },
                  onError: (err) => {
                    reportOperationError(err);
                  },
                })
              }
            >
              {deleteNode.isPending && (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              )}
              {t(($) => $.cloud_runtime.delete)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function CloudRuntimeStatusBadge({ status }: { status: string }) {
  const { t } = useT("runtimes");
  const normalized = status.toLowerCase();
  const active = new Set(["running", "success"]);
  const pending = new Set([
    "launching",
    "pending",
    "starting",
    "stopping",
    "rebooting",
    "terminating",
  ]);
  const failed = new Set([
    "failed",
    "terminated",
    "error",
    "instance_missing",
  ]);
  return (
    <Badge
      variant="secondary"
      className={cn(
        "h-5 rounded-md px-1.5 font-mono text-micro",
        active.has(normalized) && "bg-success/10 text-success",
        pending.has(normalized) && "bg-warning/10 text-warning",
        failed.has(normalized) && "bg-destructive/10 text-destructive",
      )}
    >
      {normalized === "instance_missing"
        ? t(($) => $.cloud_runtime.status.instance_missing)
        : status || "unknown"}
    </Badge>
  );
}

// Local Fleet's CreateNodeRequest accepts exactly name, spec_id and
// idempotency_key and decodes with DisallowUnknownFields, so no EC2-shaped
// field may join this payload.
function dockerCreateRequest(
  name: string | undefined,
  specId: string,
): CreateCloudRuntimeNodeRequest {
  return {
    ...(name ? { name } : {}),
    spec_id: specId,
    idempotency_key: newCloudRuntimeIdempotencyKey(),
  };
}

// EC2-shaped providers keep the pre-capability request body; spec ids are a
// Docker Fleet concept and must not leak into it. Returns null on invalid disk
// size so the caller can surface the validation message.
function ec2CreateRequest(
  name: string | undefined,
  spec: string,
  diskSizeGB: string,
): CreateCloudRuntimeNodeRequest | null {
  const diskSize = diskSizeGB.trim()
    ? Number(diskSizeGB.trim())
    : DEFAULT_DISK_SIZE_GB;
  if (!Number.isInteger(diskSize) || diskSize <= 0) return null;
  return {
    instance_type: spec,
    ...(name ? { name } : {}),
    disk_size_gb: diskSize,
  };
}

function valueOrUndefined(value: string): string | undefined {
  const trimmed = value.trim();
  return trimmed ? trimmed : undefined;
}

function formatDateTime(value: string): string | null {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return null;
  return new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}
