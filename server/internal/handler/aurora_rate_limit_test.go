package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/middleware"
)

// The Aurora generation creation endpoint is guarded by
// middleware.RateLimitByUser (wired in cmd/server/router.go), a per-user
// fixed-window gate keyed on X-User-ID. The handler tests cannot reach the
// router without an import cycle, so these tests drive the middleware directly
// with the same shape the route mounts it — the next handler stands in for
// CreateAuroraGeneration, whose 201 marks a request that passed the gate.

var auroraRateLimitNext = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusCreated)
})

func auroraRateLimitRequest(userID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/aurora/generations", nil)
	req.Header.Set("X-User-ID", userID)
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

func TestAuroraRateLimit_NoUserPassesThrough(t *testing.T) {
	gate := middleware.RateLimitByUser(newRedisTestClient(t), 1, time.Minute)
	h := gate(auroraRateLimitNext)

	req := httptest.NewRequest(http.MethodPost, "/api/aurora/generations", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 (pass-through for unauthenticated), got %d", rec.Code)
	}
}
