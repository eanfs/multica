// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  MICRO_CREDITS_PER_CREDIT,
  formatCredits,
  formatElapsed,
  formatMicroCredits,
  microToCredits,
} from "./format";

describe("microToCredits", () => {
  it("converts the ledger's micro-credit unit into whole credits", () => {
    expect(microToCredits(760 * MICRO_CREDITS_PER_CREDIT)).toBe(760);
  });

  it("rounds a fractional credit to the nearest whole one", () => {
    // A settled generation charges a fraction of the reserved amount when the
    // task stops early, so the wire carries values that are not multiples of a
    // credit.
    expect(microToCredits(760_400_000)).toBe(760);
    expect(microToCredits(760_600_000)).toBe(761);
  });

  it("keeps the sign of a ledger delta", () => {
    expect(microToCredits(-760 * MICRO_CREDITS_PER_CREDIT)).toBe(-760);
  });

  it("reports an empty wallet as zero", () => {
    expect(microToCredits(0)).toBe(0);
  });
});

describe("formatCredits", () => {
  it("groups digits for the locale it is given", () => {
    expect(formatCredits(1234, "en")).toBe("1,234");
  });

  it("leaves a small amount unseparated", () => {
    expect(formatCredits(760, "en")).toBe("760");
  });
});

describe("formatMicroCredits", () => {
  it("converts and groups together, so a screen cannot apply only one", () => {
    expect(formatMicroCredits(760 * MICRO_CREDITS_PER_CREDIT, "en")).toBe("760");
    expect(formatMicroCredits(1234 * MICRO_CREDITS_PER_CREDIT, "en")).toBe(
      "1,234",
    );
  });
});

describe("formatElapsed", () => {
  it("reports a sub-minute duration in seconds", () => {
    expect(formatElapsed(30_000, "en")).toBe("30s");
  });

  it("joins the units a duration actually has", () => {
    expect(formatElapsed(150_000, "en")).toBe("2m 30s");
  });

  it("adds hours only once the duration reaches one", () => {
    expect(formatElapsed(3_900_000, "en")).toBe("1h 5m");
  });

  it("localizes the unit names through Intl", () => {
    expect(formatElapsed(150_000, "zh-Hans")).toBe("2分钟 30秒");
  });

  it("describes an empty duration as zero seconds rather than nothing", () => {
    expect(formatElapsed(0, "en")).toBe("0s");
  });
});
