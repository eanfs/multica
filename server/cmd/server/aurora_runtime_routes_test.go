package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestManagedRuntimeRegisterRouteSkipsDaemonAuth is the wiring half of the
// handler tests in internal/handler: those call ManagedRuntimeRegister directly
// and so pass whether or not the router mounts it OUTSIDE the DaemonAuth group
// (the whole point of the endpoint — it authenticates by the shared
// AURORA_SANDBOX_TOKEN, not a daemon token). This reaches the route through the
// real router with no Authorization header and asserts it is not the
// daemon-token gate answering.
func TestManagedRuntimeRegisterRouteSkipsDaemonAuth(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}

	req, err := http.NewRequest(http.MethodPost, testServer.URL+"/api/daemon/managed/register",
		strings.NewReader(`{"workspace_id":"00000000-0000-0000-0000-000000000000"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// If the route were under DaemonAuth, a missing Authorization header would
	// get 401 "missing authorization header". The managed route is a sibling of
	// that group and instead fails closed on its own credential: 403 when
	// AURORA_SANDBOX_TOKEN is unset, or 401 "invalid sandbox token" when set.
	if strings.Contains(string(body), "missing authorization header") {
		t.Fatalf("managed register route appears gated by DaemonAuth: status=%d body=%s", resp.StatusCode, body)
	}
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 403 (unconfigured) or 401 (bad sandbox token): %s", resp.StatusCode, body)
	}
}
