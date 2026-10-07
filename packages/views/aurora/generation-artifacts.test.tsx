import { describe, expect, it } from "vitest";
import type { AuroraAsset } from "@multica/core/aurora";
import { isPreviewableImage } from "./generation-artifacts";

function image(mediaUrl: AuroraAsset["mediaUrl"]): AuroraAsset {
  return {
    id: "asset-1",
    generationId: "gen-1",
    kind: "image",
    mediaUrl,
    format: "png",
    createdAt: "2026-09-23T00:00:00Z",
  };
}

describe("isPreviewableImage", () => {
  it("accepts an image with an absolute http(s) URL", () => {
    expect(isPreviewableImage(image("https://cdn.test/poster.png"))).toBe(true);
    expect(isPreviewableImage(image("http://cdn.test/poster.png"))).toBe(true);
  });

  const unusable: [string, AuroraAsset["mediaUrl"]][] = [
    // The server can emit an empty pointer (valid-empty becomes &"") as well as
    // no pointer at all; both mean "no file to show".
    ["missing", null],
    ["empty", ""],
    ["relative", "/files/poster.png"],
    ["scheme-relative", "//cdn.test/poster.png"],
    ["non-http", "data:image/png;base64,AAAA"],
    ["javascript", "javascript:alert(1)"],
  ];

  it.each(unusable)("rejects an image whose URL is %s", (_label, mediaUrl) => {
    expect(isPreviewableImage(image(mediaUrl))).toBe(false);
  });

  it("rejects a non-image asset even when it carries a URL", () => {
    const video: AuroraAsset = {
      ...image("https://cdn.test/clip.mp4"),
      kind: "video",
    };
    expect(isPreviewableImage(video)).toBe(false);
  });
});
