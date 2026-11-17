import { describe, expect, it } from "vitest";
import { compareVersions, parseVersion } from "@multica/core/whats-new";
import { WHATS_NEW_DECK_SINCE, WHATS_NEW_RELEASES } from "./releases";

describe("WHATS_NEW_RELEASES", () => {
  it("gives every release since the deck at least one highlight with hero art", () => {
    const since = parseVersion(WHATS_NEW_DECK_SINCE)!;
    const missing = WHATS_NEW_RELEASES.filter(
      (r) => compareVersions(parseVersion(r.version)!, since) >= 0 && !r.highlights.some((h) => h.hero),
    ).map((r) => r.version);
    expect(missing).toEqual([]);
  });

  it("documents the local Docker cloud runtime in 0.11.0", () => {
    const versions = WHATS_NEW_RELEASES.map((r) => r.version);
    expect(versions[0]).toBe("0.11.0");
    expect(versions).toContain("0.10.0");

    const release = WHATS_NEW_RELEASES.find((r) => r.version === "0.11.0");
    expect(release?.date).toBe("2026-11-17");
    expect(release?.highlights.map((h) => h.id)).toContain(
      "local-docker-cloud-runtime",
    );
    const text = (release?.highlights ?? [])
      .map((h) => `${h.copy.en.title} ${h.copy.en.body}`)
      .join("\n");
    expect(text).toContain("Cloud Runtime");
    expect(text).toContain("fleet-credentials.json");

    // Upstream released 0.10.0; it must keep its own date and must not carry
    // our local-runtime highlight.
    const upstream = WHATS_NEW_RELEASES.find((r) => r.version === "0.10.0");
    expect(upstream?.date).toBe("2026-11-16");
    expect(upstream?.highlights.map((h) => h.id)).not.toContain(
      "local-docker-cloud-runtime",
    );
  });
});
