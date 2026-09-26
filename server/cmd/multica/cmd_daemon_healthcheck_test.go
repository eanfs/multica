package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// setManagedHealthcheckFlags reuses the real command so the test exercises the
// same flag names the image HEALTHCHECK passes to the binary.
func setManagedHealthcheckFlags(t *testing.T, url string, maxAge string) {
	t.Helper()
	// cobra supplies the context during Execute; these tests call the runner
	// directly, so set the same non-nil context the binary would have.
	daemonManagedHealthcheckCmd.SetContext(context.Background())
	if err := daemonManagedHealthcheckCmd.Flags().Set("url", url); err != nil {
		t.Fatalf("set url: %v", err)
	}
	if err := daemonManagedHealthcheckCmd.Flags().Set("max-age", maxAge); err != nil {
		t.Fatalf("set max-age: %v", err)
	}
}

func managedHealthServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func managedHealthBody(t *testing.T, daemonStatus string, heartbeat *time.Time) string {
	t.Helper()
	payload := map[string]any{"status": daemonStatus}
	if heartbeat != nil {
		payload["managed"] = map[string]any{"last_heartbeat_at": heartbeat.UTC().Format(time.RFC3339Nano)}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal health payload: %v", err)
	}
	return string(raw)
}

func TestManagedHealthcheckGate(t *testing.T) {
	fresh := time.Now().UTC().Add(-time.Second)
	stale := time.Now().UTC().Add(-10 * time.Minute)

	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "fresh heartbeat is healthy", status: http.StatusOK, body: managedHealthBody(t, "running", &fresh)},
		{name: "stale heartbeat fails", status: http.StatusOK, body: managedHealthBody(t, "running", &stale), wantErr: "stale"},
		{name: "starting is not healthy", status: http.StatusOK, body: managedHealthBody(t, "starting", &fresh), wantErr: "daemon status"},
		{name: "missing managed block fails", status: http.StatusOK, body: managedHealthBody(t, "running", nil), wantErr: "no acknowledged heartbeat"},
		{name: "non-200 fails", status: http.StatusInternalServerError, body: "boom", wantErr: "HTTP 500"},
		{name: "malformed body fails", status: http.StatusOK, body: "not json", wantErr: "decode response"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := managedHealthServer(t, tc.status, tc.body)
			setManagedHealthcheckFlags(t, srv.URL, "90s")
			err := runDaemonManagedHealthcheck(daemonManagedHealthcheckCmd, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected healthy, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestManagedHealthcheckRejectsBadFlags(t *testing.T) {
	setManagedHealthcheckFlags(t, "", "90s")
	if err := runDaemonManagedHealthcheck(daemonManagedHealthcheckCmd, nil); err == nil || !strings.Contains(err.Error(), "--url is required") {
		t.Fatalf("missing url: got %v", err)
	}

	setManagedHealthcheckFlags(t, "http://127.0.0.1:1/health", "0s")
	if err := runDaemonManagedHealthcheck(daemonManagedHealthcheckCmd, nil); err == nil || !strings.Contains(err.Error(), "--max-age must be positive") {
		t.Fatalf("zero max-age: got %v", err)
	}
}
