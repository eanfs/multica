import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useWorkspaceId } from "../hooks";
import {
  createAuroraCheckout,
  createAuroraGeneration,
  createAuroraTopupCheckout,
  deleteAuroraAsset,
} from "./api";
import { auroraKeys, auroraWalletKeys } from "./queries";
import type {
  CreateAuroraCheckoutRequest,
  CreateAuroraGenerationRequest,
  CreateAuroraTopupCheckoutRequest,
} from "./types";

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

/**
 * Starts a subscription checkout and sends the browser to Stripe.
 *
 * The navigation is part of the mutation rather than a `useEffect` on the
 * result: the checkout URL is a one-shot destination, and keeping it out of
 * component state means a re-render cannot navigate a second time.
 *
 * On success the wallet and the plan are invalidated. The purchase completes on
 * Stripe's origin, so this client will not see the webhook that grants the
 * credits — the invalidation is what makes the return trip re-read both, and
 * `auroraSubscriptionOptions`' stale-time covers the case where the webhook
 * lands a moment later.
 *
 * A 409 (a live subscription already exists) and a 503 (no Stripe
 * configuration) are left to the caller: neither is retryable, and the screen
 * has copy for both.
 */
export function useCreateAuroraCheckout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (request: CreateAuroraCheckoutRequest) =>
      createAuroraCheckout(request),
    onSuccess: (checkoutUrl) => {
      if (checkoutUrl) navigateToCheckout(checkoutUrl);
    },
    onSettled: () => {
      // The plan the card names, and the wallet — the webhook grants the first
      // month's credits, so both move. The pack catalogue does not, so it is
      // left alone.
      qc.invalidateQueries({ queryKey: auroraWalletKeys.subscription() });
      qc.invalidateQueries({ queryKey: auroraWalletKeys.balance() });
    },
  });
}

/** Starts a one-time credit purchase. Same shape as the subscription one. */
export function useCreateAuroraTopupCheckout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (request: CreateAuroraTopupCheckoutRequest) =>
      createAuroraTopupCheckout(request),
    onSuccess: (checkoutUrl) => {
      if (checkoutUrl) navigateToCheckout(checkoutUrl);
    },
    onSettled: () => {
      // Only the wallet moves: a top-up buys credits, not a plan.
      qc.invalidateQueries({ queryKey: auroraWalletKeys.balance() });
      qc.invalidateQueries({ queryKey: auroraWalletKeys.transactions() });
    },
  });
}

/**
 * Hands the browser to Stripe's hosted checkout.
 *
 * A full-page navigation, not a router push: the destination is another origin,
 * so it is outside every adapter's route table. `assign` is used rather than
 * `open` so the current page stays in history and the back button returns the
 * user to the plan screen if they abandon the purchase.
 */
function navigateToCheckout(url: string) {
  if (typeof window !== "undefined") window.location.assign(url);
}
