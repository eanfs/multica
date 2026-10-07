"use client";

import { useCallback } from "react";
import { toast } from "sonner";
import { downloadAuroraAsset, type AuroraAsset } from "@multica/core/aurora";
import { useT } from "../i18n";

/**
 * Returns a callback that downloads one Aurora asset.
 *
 * The shared core helper owns the authenticated request and the
 * redirect-vs-stream decision; this binding adds the app's failure report so a
 * refused download (a 401, a missing object) surfaces as a toast instead of a
 * silent no-op or a raw error page.
 */
export function useAuroraAssetDownload(): (
  asset: AuroraAsset,
) => Promise<void> {
  const { t } = useT("aurora");
  return useCallback(
    async (asset: AuroraAsset) => {
      try {
        await downloadAuroraAsset(asset);
      } catch {
        toast.error(t(($) => $.works.download_failed));
      }
    },
    [t],
  );
}
