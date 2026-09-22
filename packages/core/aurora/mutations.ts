import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useWorkspaceId } from "../hooks";
import { createAuroraGeneration, deleteAuroraAsset } from "./api";
import { auroraKeys, auroraWalletKeys } from "./queries";
import type { CreateAuroraGenerationRequest } from "./types";

/**
 * Enqueues one generation.
 *
 * The create call reserves the skill's credits before it returns, so the
 * balance on screen is stale the moment it settles — and it is just as stale
 * when the call is rejected: a 402 means the wallet was already short when the
 * request left, and the number the user is looking at was read earlier still.
 * That is why the wallet is invalidated on settle rather than on success.
 *
 * The 402 itself is left to the caller: `isAuroraInsufficientCreditsError`
 * (./api) tells the composer to show the top-up prompt. Nothing is rolled back
 * here because nothing was applied optimistically — a generation is created by
 * the server, not by the screen.
 */
export function useCreateAuroraGeneration() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (request: CreateAuroraGenerationRequest) =>
      createAuroraGeneration(request),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: auroraWalletKeys.all() });
      // Covers both the generation list and any cached detail, since a
      // generation's status and assets move together.
      qc.invalidateQueries({ queryKey: auroraKeys.generations(wsId) });
    },
  });
}

/**
 * Removes one asset from the library.
 *
 * Not optimistic, for the reason CLAUDE.md gives for deletes generally and a
 * second one specific to assets: the server also drops the stored object, so a
 * row rolled back into the list after a failed delete would offer the user a
 * file that is already gone. The row disappears when the server confirms it.
 */
export function useDeleteAuroraAsset() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (assetId: string) => deleteAuroraAsset(assetId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: auroraKeys.assets(wsId) });
      // The same asset is listed inside its generation's detail.
      qc.invalidateQueries({ queryKey: auroraKeys.generations(wsId) });
    },
  });
}
