// @vitest-environment node
import { describe, expect, it } from "vitest";
import { parseWithFallback } from "../api/schema";
import { CloudRuntimeCapabilitiesSchema } from "../api/schemas";
import { EMPTY_CLOUD_RUNTIME_CAPABILITIES, supportsNodeAction } from "./cloud-runtime-capabilities";

describe("cloud runtime action capabilities", () => {
  it("unknown provider disables operations at the schema boundary", () => {
    const caps = parseWithFallback(
      { provider: "future", operations: ["delete"] },
      CloudRuntimeCapabilitiesSchema,
      EMPTY_CLOUD_RUNTIME_CAPABILITIES,
      { endpoint: "GET /api/cloud-runtime/" },
    );
    expect(caps.provider).toBe("unknown");
    expect(caps.operations).toEqual([]);
    expect(supportsNodeAction({ ...caps, operations: ["delete"] }, "delete")).toBe(false);
  });
  it("permits only known advertised actions for known providers", () => {
    for (const provider of ["docker", "cloud"] as const) {
      const caps = {
        ...EMPTY_CLOUD_RUNTIME_CAPABILITIES,
        provider,
        operations: ["create", "stop"] as const,
      };
      expect(supportsNodeAction({ ...caps, operations: [...caps.operations] }, "create")).toBe(
        true,
      );
      expect(supportsNodeAction({ ...caps, operations: [...caps.operations] }, "delete")).toBe(
        false,
      );
    }
  });
  it("malformed capabilities safely fall back with no selections or permissions", () => {
    const caps = parseWithFallback(
      { provider: "docker", specs: [{ id: "small", cpus: 0 }] },
      CloudRuntimeCapabilitiesSchema,
      EMPTY_CLOUD_RUNTIME_CAPABILITIES,
      { endpoint: "GET /api/cloud-runtime/" },
    );
    expect(caps).toEqual({
      provider: "unknown",
      operations: [],
      specs: [],
      persistentStorage: false,
      diskQuotaSupported: false,
    });
  });
});
