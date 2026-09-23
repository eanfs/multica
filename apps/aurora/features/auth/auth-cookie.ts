/**
 * A marker cookie the proxy can read on the next request.
 *
 * It carries no authority — the session itself is the HttpOnly cookie the
 * backend sets. This one only lets a server-side redirect decide between "send
 * them into the app" and "send them to /login" without a round-trip.
 */
const COOKIE_NAME = "multica_logged_in";

export function setLoggedInCookie() {
  document.cookie = `${COOKIE_NAME}=1; path=/; max-age=31536000; samesite=lax`;
}

export function clearLoggedInCookie() {
  document.cookie = `${COOKIE_NAME}=; path=/; max-age=0`;
}
