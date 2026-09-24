import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import {
  OAUTH_MAX_PENDING_FLOWS,
  OAUTH_PENDING_NONCES_KEY,
  beginGoogleOAuthFlow,
  generateOAuthNonce,
  verifyGoogleOAuthState,
} from "./oauth-state";

/** The carriers the login pages put in `state` before #53 added the nonce. */
const CARRIERS = [
  "platform:desktop",
  "next:/invite/abc",
  "cli_callback:http%3A%2F%2Flocalhost%3A9876%2Fcallback",
  "cli_state:xyz",
];

function pendingNonces(): Record<string, number> {
  const raw = localStorage.getItem(OAUTH_PENDING_NONCES_KEY);
  return raw ? (JSON.parse(raw) as Record<string, number>) : {};
}

describe("generateOAuthNonce", () => {
  it("returns an unguessable base64url value", () => {
    const nonce = generateOAuthNonce();
    // 32 random bytes → 43 base64url characters, no padding.
    expect(nonce).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });

  it("never repeats across many draws", () => {
    const seen = new Set(Array.from({ length: 500 }, () => generateOAuthNonce()));
    expect(seen.size).toBe(500);
  });

  it("fails closed when the platform has no secure randomness", () => {
    const original = globalThis.crypto;
    // A predictable nonce is worse than none: it would look like protection
    // while being forgeable, so generation must throw rather than degrade.
    Object.defineProperty(globalThis, "crypto", {
      configurable: true,
      value: undefined,
    });
    try {
      expect(() => generateOAuthNonce()).toThrow(/getRandomValues/);
    } finally {
      Object.defineProperty(globalThis, "crypto", {
        configurable: true,
        value: original,
      });
    }
  });
});

describe("beginGoogleOAuthFlow", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it("prefixes the carriers with a nonce field", () => {
    const state = beginGoogleOAuthFlow(CARRIERS);
    const parts = state.split(",");

    expect(parts[0]).toMatch(/^nonce:[A-Za-z0-9_-]{43}$/);
    expect(parts.slice(1)).toEqual(CARRIERS);
  });

  it("omits empty carriers and still emits the nonce", () => {
    expect(beginGoogleOAuthFlow(["", "next:/x", ""])).toMatch(
      /^nonce:[A-Za-z0-9_-]{43},next:\/x$/,
    );
    expect(beginGoogleOAuthFlow([])).toMatch(/^nonce:[A-Za-z0-9_-]{43}$/);
  });

  it("mints a different nonce per flow", () => {
    expect(beginGoogleOAuthFlow([])).not.toBe(beginGoogleOAuthFlow([]));
  });

  it("remembers the nonce in browser storage for the callback to compare", () => {
    const state = beginGoogleOAuthFlow([]);
    const nonce = state.slice("nonce:".length);

    expect(Object.keys(pendingNonces())).toContain(nonce);
  });
});

describe("verifyGoogleOAuthState", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("accepts the state this browser minted and hands back the carriers", () => {
    const state = beginGoogleOAuthFlow(CARRIERS);

    const result = verifyGoogleOAuthState(state);

    expect(result).toEqual({ ok: true, carriers: CARRIERS });
  });

  it("accepts a nonce-only state", () => {
    const result = verifyGoogleOAuthState(beginGoogleOAuthFlow([]));

    expect(result).toEqual({ ok: true, carriers: [] });
  });

  it("rejects a missing state", () => {
    expect(verifyGoogleOAuthState(null)).toEqual({
      ok: false,
      reason: "missing_state",
    });
    expect(verifyGoogleOAuthState("")).toEqual({
      ok: false,
      reason: "missing_state",
    });
  });

  it("rejects a state that carries no nonce", () => {
    // The pre-#53 shape: a plain carrier string is no longer enough to
    // authorize an exchange.
    for (const state of [
      "next:/invite/abc",
      "platform:desktop",
      "cli_callback:http%3A%2F%2Flocalhost%3A9876%2Fcallback",
    ]) {
      expect(verifyGoogleOAuthState(state)).toEqual({
        ok: false,
        reason: "missing_nonce",
      });
    }
  });

  it("rejects an empty nonce field", () => {
    expect(verifyGoogleOAuthState("nonce:,next:/x")).toEqual({
      ok: false,
      reason: "missing_nonce",
    });
  });

  it("rejects a well-formed nonce this browser never minted", () => {
    // The login-CSRF shape: the attacker's own state, presented to a victim
    // whose storage has never seen that nonce.
    const forged = `nonce:${generateOAuthNonce()},next:/invite/abc`;

    expect(verifyGoogleOAuthState(forged)).toEqual({
      ok: false,
      reason: "unrecognized_nonce",
    });
  });

  it("rejects a nonce minted by a different browser", () => {
    const state = beginGoogleOAuthFlow(CARRIERS);
    localStorage.clear();

    expect(verifyGoogleOAuthState(state)).toEqual({
      ok: false,
      reason: "unrecognized_nonce",
    });
  });

  it("consumes the nonce so a replayed callback is rejected", () => {
    const state = beginGoogleOAuthFlow(CARRIERS);

    expect(verifyGoogleOAuthState(state).ok).toBe(true);
    expect(verifyGoogleOAuthState(state)).toEqual({
      ok: false,
      reason: "unrecognized_nonce",
    });
  });

  it("rejects a nonce that has outlived the flow window", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T00:00:00Z"));
    const state = beginGoogleOAuthFlow(CARRIERS);

    vi.setSystemTime(new Date("2026-01-01T00:11:00Z"));

    expect(verifyGoogleOAuthState(state)).toEqual({
      ok: false,
      reason: "unrecognized_nonce",
    });
  });

  it("leaves other in-flight flows usable when one callback is rejected", () => {
    const first = beginGoogleOAuthFlow(["next:/one"]);
    const second = beginGoogleOAuthFlow(["next:/two"]);

    expect(
      verifyGoogleOAuthState(`nonce:${generateOAuthNonce()},next:/evil`).ok,
    ).toBe(false);

    expect(verifyGoogleOAuthState(first)).toEqual({
      ok: true,
      carriers: ["next:/one"],
    });
    expect(verifyGoogleOAuthState(second)).toEqual({
      ok: true,
      carriers: ["next:/two"],
    });
  });

  it("bounds how many flows one browser remembers", () => {
    const states = Array.from({ length: OAUTH_MAX_PENDING_FLOWS + 12 }, () =>
      beginGoogleOAuthFlow([]),
    );

    expect(Object.keys(pendingNonces()).length).toBeLessThanOrEqual(
      OAUTH_MAX_PENDING_FLOWS,
    );
    // The newest flow is the one a user is actually mid-way through.
    expect(verifyGoogleOAuthState(states.at(-1)!).ok).toBe(true);
  });

  it("ignores a second nonce field smuggled in behind the real one", () => {
    const state = beginGoogleOAuthFlow(["next:/invite/abc"]);
    const nonce = state.slice("nonce:".length);

    const tampered = `nonce:${generateOAuthNonce()},nonce:${nonce},next:/evil`;

    // The first field is the one compared, so an appended nonce cannot be
    // used to smuggle the real value past the check.
    expect(verifyGoogleOAuthState(tampered)).toEqual({
      ok: false,
      reason: "unrecognized_nonce",
    });
  });

  it("does not read the prototype chain as a remembered nonce", () => {
    // `pending[nonce]` alone would find `Object.prototype` for these names.
    for (const nonce of ["__proto__", "constructor", "toString"]) {
      expect(verifyGoogleOAuthState(`nonce:${nonce},next:/x`)).toEqual({
        ok: false,
        reason: "unrecognized_nonce",
      });
    }
  });

  it("reads past a __proto__ key in a hostile record", () => {
    const state = beginGoogleOAuthFlow(["next:/x"]);
    const nonce = state.split(",")[0]!.slice("nonce:".length);
    const issuedAt = (
      JSON.parse(localStorage.getItem(OAUTH_PENDING_NONCES_KEY)!) as Record<
        string,
        number
      >
    )[nonce]!;
    // JSON.parse makes `__proto__` a real own key — the shape a hostile writer
    // of this record would produce to poison a normal object.
    localStorage.setItem(
      OAUTH_PENDING_NONCES_KEY,
      `{"__proto__": {"polluted": true}, ${JSON.stringify(nonce)}: ${issuedAt}}`,
    );

    expect(verifyGoogleOAuthState(state)).toEqual({
      ok: true,
      carriers: ["next:/x"],
    });
    expect(({} as Record<string, unknown>).polluted).toBeUndefined();
  });

  it("rejects a nonce stored under a corrupted record instead of throwing", () => {
    const state = beginGoogleOAuthFlow(["next:/x"]);
    localStorage.setItem(OAUTH_PENDING_NONCES_KEY, "not json");

    expect(verifyGoogleOAuthState(state)).toEqual({
      ok: false,
      reason: "unrecognized_nonce",
    });
  });
});
