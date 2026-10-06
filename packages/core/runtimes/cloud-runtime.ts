import { queryOptions, useMutation, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { runtimeKeys } from "./queries";
import type { CloudRuntimeCapabilities } from "./cloud-runtime-capabilities";
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
  tags: Record<string, string>;
  metadata: Record<string, unknown>;
  created_at: string;
  updated_at: string;
  provider?: CloudRuntimeCapabilities["provider"];
  ready?: boolean;
  operationId?: string;
  errorCode?: string;
}

export interface ListCloudRuntimeNodesParams {
  limit?: number;
  offset?: number;
}

export interface CreateCloudRuntimeNodeRequest {
  instance_type: string;
  name?: string;
  region?: string;
  image_id?: string;
  subnet_id?: string;
  key_name?: string;
  iam_instance_profile?: string;
  disk_size_gb?: number;
  tags?: Record<string, string>;
}

export interface CreateDockerNodeRequest {
  name?: string;
  spec: string;
}

// Account resources are shared across workspace projections.
export const cloudRuntimeKeys = {
  all: () => ["cloud-runtime"] as const,
  nodes: () => [...cloudRuntimeKeys.all(), "nodes"] as const,
  capabilities: () => [...cloudRuntimeKeys.all(), "capabilities"] as const,
};

const PENDING_NODE_STATUSES = new Set([
  "queued",
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

export function cloudRuntimeNodeListOptions(_wsId: string, params?: ListCloudRuntimeNodesParams) {
  const limit = params?.limit ?? 20;
  const offset = params?.offset ?? 0;
  return queryOptions({
    queryKey: [...cloudRuntimeKeys.nodes(), { limit, offset }] as const,
    queryFn: () => api.listCloudRuntimeNodes({ limit, offset }),
    refetchInterval: (query) =>
      query.state.data?.some((node) => isCloudRuntimeNodePending(node.status)) ? 5000 : false,
    staleTime: 15 * 1000,
  });
}

export function cloudRuntimeCapabilityOptions(_wsId: string) {
  return queryOptions({
    queryKey: cloudRuntimeKeys.capabilities(),
    queryFn: () => api.getCloudRuntimeCapabilities(),
    staleTime: 15 * 1000,
  });
}

function invalidateCloudRuntime(qc: QueryClient, wsId: string) {
  return Promise.all([
    qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all() }),
    qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) }),
  ]);
}

export function useCreateCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      idempotencyKey,
      ...data
    }: (CreateCloudRuntimeNodeRequest | CreateDockerNodeRequest) & { idempotencyKey?: string }) =>
      api.createCloudRuntimeNode(data, idempotencyKey),
    onSettled: () => invalidateCloudRuntime(qc, wsId),
  });
}

export interface CloudRuntimeNodeActionIntent {
  instanceId: string;
  idempotencyKey: string;
}

export function useStartCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ instanceId, idempotencyKey }: CloudRuntimeNodeActionIntent) =>
      api.startCloudRuntimeNode(instanceId, idempotencyKey),
    onSettled: () => invalidateCloudRuntime(qc, wsId),
  });
}

export function useStopCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ instanceId, idempotencyKey }: CloudRuntimeNodeActionIntent) =>
      api.stopCloudRuntimeNode(instanceId, idempotencyKey),
    onSettled: () => invalidateCloudRuntime(qc, wsId),
  });
}

export function useRebootCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ instanceId, idempotencyKey }: CloudRuntimeNodeActionIntent) =>
      api.rebootCloudRuntimeNode(instanceId, idempotencyKey),
    onSettled: () => invalidateCloudRuntime(qc, wsId),
  });
}

export function useDeleteCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    // Hosted callers may omit a key, as the existing API contract allows.
    mutationFn: (intent: string | CloudRuntimeNodeActionIntent) =>
      typeof intent === "string"
        ? api.deleteCloudRuntimeNode(intent)
        : api.deleteCloudRuntimeNode(intent.instanceId, intent.idempotencyKey),
    onSettled: () => invalidateCloudRuntime(qc, wsId),
  });
}
