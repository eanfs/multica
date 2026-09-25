package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestManagedRuntimeEnrollRouteSkipsDaemonAuth is the wiring half of the handler
// tests in internal/handler: those call ManagedRuntimeEnroll directly and so
// pass whether or not the router mounts it OUTSIDE the DaemonAuth group. This
// reaches the route through the real router with no Authorization header and
// asserts it is not the daemon-token gate answering. With no enrollment service
// wired (the test router's state) it fails closed on its own credential: the
// feature-disabled 403, not DaemonAuth's "missing authorization header" 401.
func TestManagedRuntimeEnrollRouteSkipsDaemonAuth(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}

	status, body := postManagedEnroll(t, testServer, "/api/daemon/managed/enroll", "")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (enrollment not configured): %s", status, body)
	}
	if !strings.Contains(body, "aurora_sandbox_not_configured") {
		t.Fatalf("body = %s, want the feature-disabled code aurora_sandbox_not_configured", body)
	}
	if strings.Contains(body, "missing authorization header") {
		t.Fatalf("managed enroll route appears gated by DaemonAuth: %s", body)
	}
}

// TestManagedRuntimeRegisterRemoved proves the global shared-token contract is
// gone: the old route is unregistered, and the old AURORA_SANDBOX_TOKEN value is
// not a credential for its replacement even when the enrollment service is
// wired. The router is rebuilt locally because the shared test server is built
// without a fleet integration and so cannot reach the 401 branch.
func TestManagedRuntimeRegisterRemoved(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	const oldToken = "aurora-legacy-shared-sandbox-token"
	t.Setenv("AURORA_SANDBOX_TOKEN", oldToken)

	hub := realtime.NewHub()
	go hub.Run()
	router, h := NewRouterWithOptions(testPool, hub, events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	h.SandboxEnrollment = aurora.NewSandboxEnrollmentService(testPool, db.New(testPool), nil)
	server := httptest.NewServer(router)
	defer server.Close()

	// The route is gone: with a credential DaemonAuth accepts, no leaf under
	// the daemon group matches the path and chi's subrouter answers 404.
	if status, body := postManagedEnroll(t, server, "/api/daemon/managed/register", "Bearer "+testToken); status != http.StatusNotFound {
		t.Fatalf("removed managed register route status = %d, want 404: %s", status, body)
	}

	// The same path with no credential is now the daemon group's 401, proof it
	// is no longer the unauthenticated managed endpoint.
	if status, body := postManagedEnroll(t, server, "/api/daemon/managed/register", ""); status != http.StatusUnauthorized || !strings.Contains(body, "missing authorization header") {
		t.Fatalf("removed managed register route (no credential) = %d %s, want the DaemonAuth 401", status, body)
	}

	// The old global secret is not a credential for the replacement endpoint.
	if status, body := postManagedEnroll(t, server, "/api/daemon/managed/enroll", "Bearer "+oldToken); status != http.StatusUnauthorized {
		t.Fatalf("enroll with the old AURORA_SANDBOX_TOKEN = %d, want 401: %s", status, body)
	}
}

// postManagedEnroll POSTs to path on server and returns the status and body. An
// empty auth sends no Authorization header.
func postManagedEnroll(t *testing.T, server *httptest.Server, path, auth string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}
