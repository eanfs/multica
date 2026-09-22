package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The Aurora generation creation endpoint is guarded by
// middleware.RateLimitByUser (wired in cmd/server/router.go), a per-user
// fixed-window gate keyed on the workspace middleware's resolved member. The
// handler tests cannot reach the router without an import cycle, so these
// tests drive the middleware directly with the same shape the route mounts it
// — the next handler stands in for CreateAuroraGeneration, whose 201 marks a
// request that passed the gate.

var auroraRateLimitNext = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusCreated)
})

// auroraRateLimitRequest builds a request whose context already carries a
// resolved workspace member, matching what RequireWorkspaceMember injects
// before the route-level gate runs.
func auroraRateLimitRequest(userID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/aurora/generations", nil)
	req = req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, db.Member{
		UserID: parseUUID(userID),
	}))
	return req
}

func TestAuroraRateLimit_AllowsUnderLimit(t *testing.T) {
	gate := middleware.RateLimitByUser(newRedisTestClient(t), 3, time.Minute)
	h := gate(auroraRateLimitNext)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, auroraRateLimitRequest(testUserID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("request %d: expected 201, got %d", i+1, rec.Code)
		}
	}
}

func TestAuroraRateLimit_BlocksOverLimit(t *testing.T) {
	gate := middleware.RateLimitByUser(newRedisTestClient(t), 2, time.Minute)
	h := gate(auroraRateLimitNext)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, auroraRateLimitRequest(testUserID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("request %d: expected 201, got %d", i+1, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, auroraRateLimitRequest(testUserID))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("expected Retry-After=60, got %q", got)
	}
}

func TestAuroraRateLimit_ScopesPerUser(t *testing.T) {
	gate := middleware.RateLimitByUser(newRedisTestClient(t), 1, time.Minute)
	h := gate(auroraRateLimitNext)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, auroraRateLimitRequest(testUserID))
	if first.Code != http.StatusCreated {
		t.Fatalf("first user request: expected 201, got %d", first.Code)
	}

	// A second account gets its own budget.
	other := httptest.NewRecorder()
	h.ServeHTTP(other, auroraRateLimitRequest("00000000-0000-0000-0000-000000000002"))
	if other.Code != http.StatusCreated {
		t.Fatalf("second user first request: expected 201, got %d", other.Code)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, auroraRateLimitRequest(testUserID))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("first user second request: expected 429, got %d", rec.Code)
	}
}

func TestAuroraRateLimit_RecoversAfterWindow(t *testing.T) {
	// The fixed-window key's TTL is the window truncated to whole seconds
	// (see the Lua script in middleware/ratelimit.go), so the recovery test
	// uses a one-second window and sleeps past it.
	gate := middleware.RateLimitByUser(newRedisTestClient(t), 1, time.Second)
	h := gate(auroraRateLimitNext)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, auroraRateLimitRequest(testUserID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first: expected 201, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, auroraRateLimitRequest(testUserID))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second within window: expected 429, got %d", rec.Code)
	}

	time.Sleep(1200 * time.Millisecond)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, auroraRateLimitRequest(testUserID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("after window: expected 201, got %d", rec.Code)
	}
}

// TestAuroraRateLimit_MissingMemberFailsClosed pins the security contract:
// RateLimitByUser must not rate-limit (and thereby admit) a request whose
// context carries no resolved member, because there is no verified identity to
// key on. In the real router this branch is unreachable — the route sits behind
// RequireWorkspaceMember — but a mis-mounted gate must fail closed, not open.
func TestAuroraRateLimit_MissingMemberFailsClosed(t *testing.T) {
	gate := middleware.RateLimitByUser(newRedisTestClient(t), 1, time.Minute)
	h := gate(auroraRateLimitNext)

	req := httptest.NewRequest(http.MethodPost, "/api/aurora/generations", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (fail closed without a member), got %d", rec.Code)
	}
}
