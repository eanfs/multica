// @vitest-environment node
import { describe, expect, it } from "vitest";
import { isAuroraGenerationTerminal } from "./types";

describe("isAuroraGenerationTerminal", () => {
  it("treats completed and failed as final", () => {
    expect(isAuroraGenerationTerminal("completed")).toBe(true);
    expect(isAuroraGenerationTerminal("failed")).toBe(true);
  });

  it("keeps polling the two in-flight statuses", () => {
    expect(isAuroraGenerationTerminal("queued")).toBe(false);
    expect(isAuroraGenerationTerminal("running")).toBe(false);
  });

  it("keeps polling an unrecognised status rather than assuming it is final", () => {
    // A newer server could add a status. Reading it as final would stop the
    // poll before the generation's assets existed; reading it as in-flight
    // costs one request every three seconds until the server settles it.
    expect(isAuroraGenerationTerminal("paused-for-review")).toBe(false);
  });

  it("keeps polling when the detail body could not be read", () => {
    expect(isAuroraGenerationTerminal(undefined)).toBe(false);
    expect(isAuroraGenerationTerminal("")).toBe(false);
  });
});
