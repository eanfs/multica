/**
 * CSRF nonce for the Google OAuth redirect flow (issue #53).
 *
 * `state` used to be a pure data carrier: the login page packed `next:` /
 * `platform:desktop` / `cli_callback:` into it and the callback page read them
 * back. Nothing tied the authorization code being exchanged to the browser that
 * started the flow, so a callback URL holding the attacker's own code could be
 * handed to a victim and the victim's browser would happily exchange it —
 * login-CSRF.
 *
 * The flow now mints an unguessable nonce, remembers it in this browser's
 * storage, and the callback compares the returned nonce against that record
 * *before* the code is exchanged. The nonce rides in the same comma-joined
 * `state` string as a leading `nonce:` field, so every existing carrier keeps
 * working and the callback's carrier parsing is unchanged.
 *
 * Why browser storage is the right place: the attacker can read the state they
 * caused — it is in their own URL — but cannot write to the victim's storage
 * for this origin. A state the attacker chose therefore never matches a nonce
 * this browser minted. `localStorage` rather than `sessionStorage` so the
 * record survives whichever tab the browser lands the callback in; the TTL
 * bounds how long a stale record stays usable.
 */

/**
 * Storage key holding the pending flows as `{ [nonce]: issuedAt }`.
 *
 * Exported because it is a persistence contract, not an implementation detail:
 * a signed-out browser must not be able to accumulate records here.
 */
export const OAUTH_PENDING_NONCES_KEY = "multica.oauth.pending_nonces";

/**
 * How many flows one browser remembers. Bounds the record for a tab that
 * starts flow after flow, and evicts oldest-first — the newest flow is the one
 * a user is actually mid-way through.
 */
export const OAUTH_MAX_PENDING_FLOWS = 8;

/** A Google round-trip that takes longer than this has been abandoned. */
const NONCE_TTL_MS = 10 * 60 * 1000;

/** 256 bits: far beyond guessing, and short enough to keep `state` tidy. */
const NONCE_BYTES = 32;

const NONCE_FIELD = "nonce:";

export type OAuthStateFailure =
  /** No `state` came back at all. */
  | "missing_state"
  /** `state` came back but carried no nonce field. */
  | "missing_nonce"
  /** The nonce is well-formed but this browser never minted it (or it is spent). */
  | "unrecognized_nonce";

export type OAuthStateCheck =
  | { ok: true; carriers: string[] }
  | { ok: false; reason: OAuthStateFailure };

/**
 * Mint an unguessable nonce. Throws when the platform cannot provide secure
 * randomness: a predictable nonce would look like protection without being it,
 * so there is nothing safe to fall back to.
 */
export function generateOAuthNonce(): string {
  const cryptoObj = globalThis.crypto;
  if (!cryptoObj?.getRandomValues) {
    throw new Error(
      "OAuth state nonce requires crypto.getRandomValues for secure randomness",
    );
  }

  const bytes = new Uint8Array(NONCE_BYTES);
  cryptoObj.getRandomValues(bytes);

  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/**
 * Start a Google OAuth flow: mint a nonce, remember it for the callback to
 * compare against, and return the `state` string to hand to Google.
 *
 * `carriers` are the existing `next:` / `platform:desktop` / `cli_callback:` /
 * `cli_state:` fields. Call this from the click handler, not during render —
 * the nonce has to be minted once per flow, at flow start.
 */
export function beginGoogleOAuthFlow(carriers: readonly string[]): string {
  const nonce = generateOAuthNonce();
  rememberNonce(nonce, Date.now());

  return [`${NONCE_FIELD}${nonce}`, ...carriers.filter(Boolean)].join(",");
}

/**
 * Check the `state` Google handed back, before any authorization code is
 * exchanged.
 *
 * Accepts only a state whose nonce this browser minted and has not spent. On
 * success the nonce is consumed, so a captured callback URL cannot be replayed;
 * on failure nothing is consumed, leaving other in-flight flows usable.
 */
export function verifyGoogleOAuthState(state: string | null): OAuthStateCheck {
  if (!state) return { ok: false, reason: "missing_state" };

  const parts = state.split(",");
  // The first `nonce:` field is authoritative: a second one appended behind it
  // cannot smuggle the real value past the comparison.
  const noncePart = parts.find((part) => part.startsWith(NONCE_FIELD));
  const nonce = noncePart?.slice(NONCE_FIELD.length) ?? "";
  if (!nonce) return { ok: false, reason: "missing_nonce" };

  const pending = readPendingNonces(Date.now());
  if (!Object.prototype.hasOwnProperty.call(pending, nonce)) {
    return { ok: false, reason: "unrecognized_nonce" };
  }

  delete pending[nonce];
  writePendingNonces(pending);

  return {
    ok: true,
    carriers: parts.filter((part) => !part.startsWith(NONCE_FIELD)),
  };
}

// ---------------------------------------------------------------------------
// Pending-nonce record
// ---------------------------------------------------------------------------

/**
 * Null-prototype throughout: the nonce is attacker-controlled input on the
 * lookup path, so a `__proto__` key must land as an ordinary property rather
 * than reach the prototype chain. The `hasOwnProperty` check below is the
 * second half of that — `pending[nonce]` alone would find `Object.prototype`.
 */
type PendingNonces = Record<string, number>;

function emptyPendingNonces(): PendingNonces {
  return Object.create(null) as PendingNonces;
}

/**
 * `null` when the browser blocks storage (private mode, site data disabled).
 * The flow cannot be secured without somewhere to remember the nonce, so
 * callers treat a missing store as "nothing remembered" and fail closed.
 */
function pendingNonceStorage(): Storage | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

function readPendingNonces(now: number): PendingNonces {
  const store = pendingNonceStorage();
  if (!store) return emptyPendingNonces();

  let raw: string | null = null;
  try {
    raw = store.getItem(OAUTH_PENDING_NONCES_KEY);
  } catch {
    return emptyPendingNonces();
  }
  if (!raw) return emptyPendingNonces();

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    // A corrupted record is not a crash: it just means no flow is recognized,
    // which is the same answer as an empty record.
    return emptyPendingNonces();
  }
  if (!parsed || typeof parsed !== "object") return emptyPendingNonces();

  const live = emptyPendingNonces();
  for (const [nonce, issuedAt] of Object.entries(
    parsed as Record<string, unknown>,
  )) {
    if (typeof issuedAt === "number" && now - issuedAt < NONCE_TTL_MS) {
      live[nonce] = issuedAt;
    }
  }
  return live;
}

function writePendingNonces(pending: PendingNonces): void {
  const store = pendingNonceStorage();
  if (!store) return;
  try {
    store.setItem(OAUTH_PENDING_NONCES_KEY, JSON.stringify(pending));
  } catch {
    // Storage full or blocked — the flow simply will not be recognized.
  }
}

function rememberNonce(nonce: string, now: number): void {
  const pending = readPendingNonces(now);
  pending[nonce] = now;

  const byAge = Object.entries(pending).sort(([, a], [, b]) => a - b);
  for (const [stale] of byAge.slice(
    0,
    Math.max(0, byAge.length - OAUTH_MAX_PENDING_FLOWS),
  )) {
    delete pending[stale];
  }

  writePendingNonces(pending);
}
