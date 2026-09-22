import { queryOptions, useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "../hooks";
import {
  getAuroraBalance,
  getAuroraGeneration,
  listAuroraAssets,
  listAuroraGenerations,
  listAuroraSkills,
  listAuroraTransactions,
} from "./api";
import {
  AURORA_GENERATION_POLL_MS,
  isAuroraGenerationTerminal,
  type AuroraAssetsParams,
  type AuroraListParams,
} from "./types";

/**
 * Workspace-scoped Aurora keys.
 *
 * The catalog, the generations and the library are all per-workspace, so `wsId`
 * is the second segment of every key: switching workspaces must not serve one
 * workspace's generations out of another's cache.
 */
export const auroraKeys = {
  all: (wsId: string) => ["aurora", wsId] as const,
  skills: (wsId: string) => [...auroraKeys.all(wsId), "skills"] as const,
  /** The list and every detail: invalidating this refreshes both. */
  generations: (wsId: string) => [...auroraKeys.all(wsId), "generations"] as const,
  // The page params are part of the key, so a paged read cannot be served the
  // default page's rows. The "list" segment keeps a page from ever colliding
  // with a generation id.
  generationList: (wsId: string, params?: AuroraListParams) =>
    [...auroraKeys.generations(wsId), "list", params ?? {}] as const,
  generation: (wsId: string, id: string) =>
    [...auroraKeys.generations(wsId), id] as const,
  assets: (wsId: string) => [...auroraKeys.all(wsId), "assets"] as const,
  assetList: (wsId: string, params?: AuroraAssetsParams) =>
    [...auroraKeys.assets(wsId), params ?? {}] as const,
};

/**
 * Wallet keys, deliberately *not* scoped to a workspace.
 *
 * The Aurora wallet belongs to the user, not the workspace — the server reads
 * the caller's account and every workspace shows the same balance — so keying
 * these on `wsId` would refetch identical data on every workspace switch. Same
 * reasoning as `packages/core/billing/queries.ts`. The separate `"wallet"`
 * segment keeps them out of the `["aurora", wsId]` subtree, so invalidating a
 * workspace's data cannot touch the account's.
 */
export const auroraWalletKeys = {
  all: () => ["aurora", "wallet"] as const,
  balance: () => [...auroraWalletKeys.all(), "balance"] as const,
  transactions: () => [...auroraWalletKeys.all(), "transactions"] as const,
};

export function auroraSkillsOptions(wsId: string) {
  return queryOptions({
    queryKey: auroraKeys.skills(wsId),
    queryFn: () => listAuroraSkills(),
    enabled: wsId.length > 0,
    // The catalog is a server-side constant (`aurora/catalog.go`): it only
    // changes when the server is redeployed, so it is held far longer than
    // user data and refreshed on the next mount after a deploy.
    staleTime: 30 * 60 * 1000,
  });
}

export function auroraGenerationsOptions(
  wsId: string,
  params?: AuroraListParams,
) {
  return queryOptions({
    queryKey: auroraKeys.generationList(wsId, params),
    queryFn: () => listAuroraGenerations(params),
    enabled: wsId.length > 0,
  });
}

export function auroraGenerationDetailOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: auroraKeys.generation(wsId, id),
    queryFn: () => getAuroraGeneration(id),
    enabled: wsId.length > 0 && id.length > 0,
    // MVP progress: poll while the generation can still change, stop once it
    // cannot. An unreadable body parses to null, which is not terminal, so it
    // keeps polling rather than parking the screen on an empty result — the
    // next tick is what recovers it.
    refetchInterval: (query) =>
      isAuroraGenerationTerminal(query.state.data?.status)
        ? false
        : AURORA_GENERATION_POLL_MS,
  });
}

export function auroraAssetsOptions(wsId: string, params?: AuroraAssetsParams) {
  return queryOptions({
    queryKey: auroraKeys.assetList(wsId, params),
    queryFn: () => listAuroraAssets(params),
    enabled: wsId.length > 0,
  });
}

export function auroraBalanceOptions() {
  return queryOptions({
    queryKey: auroraWalletKeys.balance(),
    queryFn: () => getAuroraBalance(),
    // The wallet moves only when a generation reserves or settles credits, so
    // it is refreshed by the mutation that causes the move rather than by
    // polling for it.
    staleTime: 30 * 1000,
  });
}

export function auroraTransactionsOptions() {
  return queryOptions({
    queryKey: auroraWalletKeys.transactions(),
    queryFn: () => listAuroraTransactions(),
    staleTime: 30 * 1000,
  });
}

// The hooks below read the workspace from context instead of taking it: every
// Aurora surface renders inside the `[workspaceSlug]` layout, which is what
// establishes the workspace, so there is no case in which the caller has an id
// the provider does not. The `*Options` functions above stay wsId-first for
// callers that do hold one (prefetch, tests).

/** The catalog, including the phase-2 skills that cannot be run yet. */
export function useAuroraSkills() {
  const wsId = useWorkspaceId();
  return useQuery(auroraSkillsOptions(wsId));
}

/** The workspace's generations, newest first. */
export function useAuroraGenerations(params?: AuroraListParams) {
  const wsId = useWorkspaceId();
  return useQuery(auroraGenerationsOptions(wsId, params));
}

/** One generation with its assets, polled while it is still in flight. */
export function useAuroraGenerationDetail(id: string) {
  const wsId = useWorkspaceId();
  return useQuery(auroraGenerationDetailOptions(wsId, id));
}

/** The workspace's content library, optionally narrowed to one generation. */
export function useAuroraAssets(params?: AuroraAssetsParams) {
  const wsId = useWorkspaceId();
  return useQuery(auroraAssetsOptions(wsId, params));
}

/** The caller's wallet. */
export function useAuroraBalance() {
  return useQuery(auroraBalanceOptions());
}

/** The caller's ledger, newest first. */
export function useAuroraTransactions() {
  return useQuery(auroraTransactionsOptions());
}
