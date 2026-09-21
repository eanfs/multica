package middleware

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// rateLimitScript atomically increments the counter and sets the TTL on
// first access. Using a Lua script ensures INCR and EXPIRE cannot be
// split by a network failure — if INCR succeeds the TTL is guaranteed
// to be set, preventing a stuck key that acts as a permanent ban.
var rateLimitScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
    redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return count
`)

// ParseTrustedProxies parses a comma-separated list of CIDRs into a
// slice of *net.IPNet. Invalid entries are warned and skipped.
// Returns nil if raw is empty (default: never trust X-Forwarded-For).
func ParseTrustedProxies(raw string) []*net.IPNet {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	var nets []*net.IPNet
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(p)
		if err != nil {
			slog.Warn("ratelimit: invalid trusted proxy CIDR, skipping", "cidr", p, "error", err)
			continue
		}
		nets = append(nets, cidr)
	}
	return nets
}

// RateLimit returns a per-IP fixed-window rate limiter backed by Redis.
// If rdb is nil the middleware is a no-op (fail-open).
//
// trustedProxies controls X-Forwarded-For handling: when the direct
// connection (RemoteAddr) originates from a CIDR in the list, the
// rightmost non-trusted IP in the XFF chain is used as the client IP.
// When the list is empty (default), XFF is never consulted — only
// RemoteAddr is used. This matches the project's conservative trust
// model (see health_realtime.go).
func RateLimit(rdb redis.UniversalClient, limit int, window time.Duration, trustedProxies []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if rdb == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fixedWindowRateLimit(w, r, rdb, rateLimitKey(r.URL.Path, extractIP(r, trustedProxies)), limit, window, next)
		})
	}
}

// RateLimitByUser returns a per-user fixed-window rate limiter backed by Redis.
// It keys on the workspace middleware's resolved member (RequireWorkspaceMember
// injects a db.Member into the request context after validating the caller's
// session and workspace membership), rather than the client IP or the raw
// X-User-ID header, so the budget follows the verified account across
// workspaces and clients.
//
// It must be mounted inside a RequireWorkspaceMember-protected route group:
// when no member is present in the context the middleware fails closed rather
// than rate-limiting against an unverified identity.
//
// Like RateLimit, a nil rdb makes the middleware a no-op (fail-open).
func RateLimitByUser(rdb redis.UniversalClient, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if rdb == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			member, ok := MemberFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusInternalServerError, "rate limiter unavailable")
				return
			}
			fixedWindowRateLimit(w, r, rdb, rateLimitUserKey(r.URL.Path, uuidToString(member.UserID)), limit, window, next)
		})
	}
}

// fixedWindowRateLimit runs the fixed-window increment against key and either
// forwards to next — under the limit, or on a Redis error, where the limiter
// fails open — or writes a 429. The key already encodes the path and subject
// (IP or user id), so a Redis-error log carries both.
func fixedWindowRateLimit(w http.ResponseWriter, r *http.Request, rdb redis.UniversalClient, key string, limit int, window time.Duration, next http.Handler) {
	count, err := rateLimitScript.Run(r.Context(), rdb, []string{key}, int(window.Seconds())).Int64()
	if err != nil {
		slog.Warn("ratelimit: redis error; allowing request", "error", err, "key", key)
		next.ServeHTTP(w, r)
		return
	}
	if count > int64(limit) {
		writeRateLimited(w, window)
		return
	}
	next.ServeHTTP(w, r)
}

// writeRateLimited writes the shared 429 response (Retry-After + JSON body).
func writeRateLimited(w http.ResponseWriter, window time.Duration) {
	w.Header().Set("Retry-After", fmt.Sprintf("%d", int(window.Seconds())))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(map[string]string{"error": "too many requests"})
}

// extractIP determines the client IP for rate limiting purposes.
// It only honors X-Forwarded-For when RemoteAddr is from a trusted proxy.
func extractIP(r *http.Request, trustedProxies []*net.IPNet) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	if len(trustedProxies) > 0 {
		remoteIP := net.ParseIP(remoteHost)
		if remoteIP != nil && isTrustedProxy(remoteIP, trustedProxies) {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				// Walk right-to-left: the rightmost non-trusted entry is
				// the last hop before the trusted proxy chain.
				parts := strings.Split(xff, ",")
				for i := len(parts) - 1; i >= 0; i-- {
					candidate := net.ParseIP(strings.TrimSpace(parts[i]))
					if candidate != nil && !isTrustedProxy(candidate, trustedProxies) {
						return candidate.String()
					}
				}
			}
		}
	}

	// Default: use RemoteAddr in canonical form.
	if ip := net.ParseIP(remoteHost); ip != nil {
		return ip.String()
	}
	return remoteHost
}

func isTrustedProxy(ip net.IP, cidrs []*net.IPNet) bool {
	for _, cidr := range cidrs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// rateLimitPathSegment flattens a request path into a single key segment:
// leading slash dropped, inner slashes replaced with colons.
func rateLimitPathSegment(path string) string {
	segment := strings.TrimPrefix(path, "/")
	return strings.ReplaceAll(segment, "/", ":")
}

func rateLimitKey(path, ip string) string {
	return fmt.Sprintf("mul:ratelimit:%s:%s", rateLimitPathSegment(path), ip)
}

func rateLimitUserKey(path, userID string) string {
	return fmt.Sprintf("mul:ratelimit:user:%s:%s", rateLimitPathSegment(path), userID)
}
