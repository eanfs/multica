import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";

export interface CloudRuntimeNode {
  id: string;
  owner_id: string;
  instance_id: string;
  region: string;
  instance_type: string;
  image_id: string;
  subnet_id: string;
  name: string;
  status: string;
  /** Provider that owns the node ("docker", "ec2", ...); optional because
   *  backends predating local Docker runtime omit it. */
  provider?: string;
  /** Daemon this node is expected to host (local Fleet mode). */
  daemonId?: string;
  /** Per-node readiness signals (CLI installed, model configured, ...). */
  statusDetail?: Record<string, unknown>;
  /** Last operation error reported for the node. */
  error?: string;
  tags: Record<string, string>;
  metadata: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

export interface ListCloudRuntimeNodesParams {
  limit?: number;
  offset?: number;
}

export interface CreateCloudRuntimeNodeRequest {
  /** Docker Fleet selects its allowed node shape with this id. */
  spec_id?: string;
  /** Per-submission idempotency key; the API client also mirrors it in the
   *  Idempotency-Key header. Docker create bodies must carry exactly this,
   *  the name and spec_id — the local Fleet rejects unknown fields. */
  idempotency_key?: string;
  /** EC2-shaped providers select the allowed node shape with instance_type. */
  instance_type?: string;
  name?: string;
  region?: string;
  image_id?: string;
  subnet_id?: string;
  key_name?: string;
  iam_instance_profile?: string;
  disk_size_gb?: number;
  tags?: Record<string, string>;
}

export interface CloudRuntimeSpec {
  id: string;
  cpu: number;
  memoryMb: number;
  maxConcurrent: number;
}

export interface CloudRuntimeService {
  provider: string;
  operations: string[];
  specs: CloudRuntimeSpec[];
  persistentStorage: boolean;
  diskQuota: boolean;
}

export const EMPTY_CLOUD_RUNTIME_SERVICE: CloudRuntimeService = {
  provider: "",
  operations: [],
  specs: [],
  persistentStorage: false,
  diskQuota: false,
};

export const cloudRuntimeKeys = {
  all: (wsId: string) => ["cloud-runtime", wsId] as const,
  nodes: (wsId: string) => [...cloudRuntimeKeys.all(wsId), "nodes"] as const,
};

export const cloudRuntimeServiceKeys = {
  service: (wsId: string) => [...cloudRuntimeKeys.all(wsId), "service"] as const,
};

export function cloudRuntimeServiceOptions(wsId: string) {
  return queryOptions({
    queryKey: cloudRuntimeServiceKeys.service(wsId),
    queryFn: () => api.getCloudRuntimeService(),
    staleTime: 60 * 1000,
  });
}

// Idempotency keys only need to be unique per write. crypto.randomUUID is
// available in browsers and Node, but older React Native (Hermes) runtimes
// ship without it, so fall back to a random string instead of pulling in a
// dependency for one call.
export function newCloudRuntimeIdempotencyKey(): string {
  const uuid = globalThis.crypto?.randomUUID?.();
  if (uuid) return uuid;
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

const PENDING_NODE_STATUSES = new Set([
  "launching",
  "pending",
  "starting",
  "stopping",
  "rebooting",
  "terminating",
]);

export function isCloudRuntimeNodePending(status: string): boolean {
  return PENDING_NODE_STATUSES.has(status.toLowerCase());
}

export function cloudRuntimeNodeListOptions(
  wsId: string,
  params?: ListCloudRuntimeNodesParams,
) {
  const limit = params?.limit ?? 20;
  const offset = params?.offset ?? 0;
  return queryOptions({
    queryKey: [...cloudRuntimeKeys.nodes(wsId), { limit, offset }] as const,
    queryFn: () => api.listCloudRuntimeNodes({ limit, offset }),
    refetchInterval: (query) =>
      query.state.data?.some((node) => isCloudRuntimeNodePending(node.status))
        ? 5000
        : false,
    staleTime: 15 * 1000,
  });
}

export function useCreateCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: CreateCloudRuntimeNodeRequest) =>
      api.createCloudRuntimeNode(data),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all(wsId) });
    },
  });
}

export function useDeleteCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (instanceId: string) =>
      api.deleteCloudRuntimeNode(instanceId, newCloudRuntimeIdempotencyKey()),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all(wsId) });
    },
  });
}

export function useStartCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (instanceId: string) =>
      api.startCloudRuntimeNode(instanceId, newCloudRuntimeIdempotencyKey()),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all(wsId) });
    },
  });
}

export function useStopCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (instanceId: string) =>
      api.stopCloudRuntimeNode(instanceId, newCloudRuntimeIdempotencyKey()),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all(wsId) });
    },
  });
}

export function useRebootCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (instanceId: string) =>
      api.rebootCloudRuntimeNode(instanceId, newCloudRuntimeIdempotencyKey()),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all(wsId) });
    },
  });
}
