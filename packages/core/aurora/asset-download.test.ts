// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import {
  auroraAssetDownloadFilename,
  downloadAuroraAsset,
} from "./asset-download";

const SIGNED = "https://cdn.example.test/asset.png?Signature=s";

/** Answers every request through the documented api-client layer. */
function installResponse(response: Response): ReturnType<typeof vi.fn> {
  const requestResponse = vi.fn().mockResolvedValue(response);
  setApiInstance({ requestResponse } as unknown as ApiClient);
  return requestResponse;
}

/**
 * A Response stand-in carrying only the fields the helper reads. Response's
 * own redirected/url are read-only, so a followed redirect is built here.
 */
function responseLike(overrides: Partial<Response>): Response {
  return {
    status: 200,
    ok: true,
    redirected: false,
    url: "",
    headers: new Headers(),
    body: null,
    blob: async () => new Blob(),
    ...overrides,
  } as unknown as Response;
}

function stubObjectUrl(): { create: ReturnType<typeof vi.fn>; revoke: ReturnType<typeof vi.fn> } {
  const create = vi.fn(() => "blob:mock");
  const revoke = vi.fn();
  Object.defineProperty(URL, "createObjectURL", { configurable: true, value: create });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: revoke });
  return { create, revoke };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("downloadAuroraAsset", () => {
  it("opens a redirect Location instead of reading the body", async () => {
    const blob = vi.fn();
    const requestResponse = installResponse(
      responseLike({ status: 302, ok: false, blob, headers: new Headers({ Location: SIGNED }) }),
    );
    const open = vi.spyOn(window, "open").mockReturnValue(null);

    await downloadAuroraAsset({ id: "asset-1", format: "png" });

    // The request carries the bearer-token download route through the client.
    expect(requestResponse).toHaveBeenCalledWith(
      "/api/aurora/assets/asset-1/download",
    );
    expect(open).toHaveBeenCalledWith(SIGNED, "_blank", "noopener,noreferrer");
    // The signed URL streams from storage; the body is never buffered.
    expect(blob).not.toHaveBeenCalled();
  });

  it("opens the final URL of a followed redirect without buffering it", async () => {
    const blob = vi.fn();
    installResponse(
      responseLike({ redirected: true, url: SIGNED, blob }),
    );
    const open = vi.spyOn(window, "open").mockReturnValue(null);

    await downloadAuroraAsset({ id: "asset-1", format: "png" });

    expect(open).toHaveBeenCalledWith(SIGNED, "_blank", "noopener,noreferrer");
    expect(blob).not.toHaveBeenCalled();
  });

  it("saves a streamed body under the Content-Disposition name", async () => {
    const body = new Blob(["poster"], { type: "image/png" });
    installResponse(
      responseLike({
        headers: new Headers({
          "Content-Disposition": 'attachment; filename="poster.png"',
        }),
        blob: async () => body,
      }),
    );
    const { create, revoke } = stubObjectUrl();
    const click = vi
      .spyOn(HTMLAnchorElement.prototype, "click")
      .mockImplementation(() => {});
    const append = vi.spyOn(document.body, "appendChild");

    await downloadAuroraAsset({ id: "asset-1", format: "png" });

    expect(create).toHaveBeenCalledWith(body);
    const anchor = append.mock.calls
      .map(([node]) => node)
      .find((node): node is HTMLAnchorElement => node instanceof HTMLAnchorElement);
    expect(anchor).toBeDefined();
    expect(anchor!.download).toBe("poster.png");
    expect(click).toHaveBeenCalledOnce();
    // The object URL is released once the click has started the save.
    expect(revoke).toHaveBeenCalledWith("blob:mock");
  });

  it("rejects when the endpoint refuses the download", async () => {
    const requestResponse = vi
      .fn()
      .mockRejectedValue(new Error("missing authorization"));
    setApiInstance({ requestResponse } as unknown as ApiClient);

    await expect(
      downloadAuroraAsset({ id: "asset-1", format: "png" }),
    ).rejects.toThrow("missing authorization");
  });

  it("rejects a redirect with no usable Location", async () => {
    installResponse(responseLike({ status: 302, ok: false }));

    await expect(
      downloadAuroraAsset({ id: "asset-1", format: "png" }),
    ).rejects.toThrow("status 302");
  });
});

describe("auroraAssetDownloadFilename", () => {
  it("prefers the RFC 5987 filename over the ASCII fallback", () => {
    expect(
      auroraAssetDownloadFilename(
        "attachment; filename=\"poster.png\"; filename*=UTF-8''%E6%B5%B7%E6%8A%A5.png",
        "png",
      ),
    ).toBe("海报.png");
  });

  it("falls back to the asset format when the header has no name", () => {
    expect(auroraAssetDownloadFilename(null, "mp4")).toBe("asset.mp4");
    expect(auroraAssetDownloadFilename("attachment", "mp4")).toBe("asset.mp4");
    expect(auroraAssetDownloadFilename("attachment", null)).toBe("asset");
  });
});
