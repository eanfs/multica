import { api } from "../api";
import { auroraAssetDownloadPath } from "./api";
import type { AuroraAsset } from "./schema";

/**
 * The two shapes the Aurora download endpoint can answer with.
 *
 * The endpoint is the only thing that names the file, so the client must go
 * through it: CloudFront and presign deployments answer a 302 to a short-lived
 * signed URL, and a deployment with no signable URL streams the object itself
 * with a Content-Disposition filename. The browser cannot attach the bearer
 * token a plain navigation would need, so the request runs through the shared
 * API client (auth + workspace headers) and this helper routes the answer:
 * a redirect is opened so the object streams to disk, anything else is saved
 * from the response body.
 *
 * Throws on failure; the caller owns the user-facing report (the Aurora views
 * wrap it with their failure toast).
 */
export async function downloadAuroraAsset(
  asset: Pick<AuroraAsset, "id" | "format">,
): Promise<void> {
  const response = await api.requestResponse(auroraAssetDownloadPath(asset.id));

  const redirect = redirectTarget(response);
  if (redirect) {
    // A followed redirect already started pulling the object; drop that copy
    // before opening the URL so the file is not buffered into renderer memory.
    discardResponseBody(response);
    openDownloadedUrl(redirect);
    return;
  }

  if (!response.ok) {
    throw new Error(
      `aurora asset download failed with status ${response.status}`,
    );
  }

  const blob = await response.blob();
  saveBlob(
    blob,
    auroraAssetDownloadFilename(
      response.headers.get("Content-Disposition"),
      asset.format,
    ),
  );
}

/**
 * The URL a 3xx redirect points at, or the final URL of a followed redirect.
 *
 * The 3xx branch reads Location directly; it is what a test or a non-browser
 * fetch surfaces. A browser hides Location behind an opaque redirect when
 * `redirect: "manual"` is used, so the request follows normally and the signed
 * target is read from Response.redirected / Response.url instead.
 */
function redirectTarget(response: Response): string | null {
  if (response.status >= 300 && response.status < 400) {
    const location = response.headers.get("Location");
    return isHttpUrl(location) ? location : null;
  }
  if (response.redirected && isHttpUrl(response.url)) return response.url;
  return null;
}

/** A non-empty absolute http(s) URL — the only kind worth opening. */
function isHttpUrl(value: string | null | undefined): value is string {
  if (!value) return false;
  try {
    const url = new URL(value);
    return (
      (url.protocol === "http:" || url.protocol === "https:") &&
      url.host !== ""
    );
  } catch {
    return false;
  }
}

/** The saved name: the endpoint's Content-Disposition, else the asset format. */
export function auroraAssetDownloadFilename(
  contentDisposition: string | null,
  format: string | null | undefined,
): string {
  return (
    filenameFromContentDisposition(contentDisposition) ??
    (format ? `asset.${format}` : "asset")
  );
}

/**
 * Reads a filename from a Content-Disposition header, preferring RFC 5987
 * `filename*` over the ASCII fallback. Returns null when the header carries no
 * usable name, so the caller can fall back to the asset format.
 */
function filenameFromContentDisposition(value: string | null): string | null {
  if (!value) return null;
  const extended = /filename\*\s*=\s*[^']*''([^;]+)/i.exec(value);
  if (extended?.[1]) {
    try {
      const decoded = decodeURIComponent(extended[1].trim());
      if (decoded) return decoded;
    } catch {
      // Malformed percent-encoding: fall through to the ASCII filename.
    }
  }
  const basic = /filename\s*=\s*"?([^";]+)"?/i.exec(value);
  const name = basic?.[1]?.trim();
  return name ? name : null;
}

/** Stops the body of a followed redirect so it is not downloaded twice. */
function discardResponseBody(response: Response): void {
  const body = response.body;
  if (!body) return;
  void body.cancel().catch(() => {});
}

/** Opens a signed URL in a new context; the object streams instead of buffering. */
function openDownloadedUrl(url: string): void {
  if (typeof window === "undefined") return;
  window.open(url, "_blank", "noopener,noreferrer");
}

/** Saves a streamed body under its endpoint-provided filename. */
function saveBlob(blob: Blob, filename: string): void {
  if (typeof document === "undefined") return;
  const objectUrl = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = objectUrl;
  anchor.download = filename;
  anchor.rel = "noopener";
  anchor.style.display = "none";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(objectUrl);
}
