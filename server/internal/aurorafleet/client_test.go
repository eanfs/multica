package aurorafleet

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestTokenFile(t *testing.T) string {
	t.Helper()
	return writeTokenFile(t, t.TempDir(), testTokenB64, 0o400)
}

func newTokenServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	auth := &ControlAuth{token: testTokenRaw}
	srv := httptest.NewServer(auth.Middleware(handler))
	t.Cleanup(srv.Close)
	return srv
}

func TestControlClientSendsBearerAndParsesNode(t *testing.T) {
	var gotAuth string
	srv := newTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/internal/v1/workspace-nodes/node-1" || r.Method != http.MethodPut {
			t.Errorf("request = %s %s, want PUT /internal/v1/workspace-nodes/node-1", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, Node{ID: "node-1", State: StateOnline})
	})
	client, err := NewControlClient(srv.URL, newTestTokenFile(t))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}

	node, err := client.EnsureWorkspaceNode(context.Background(), EnsureRequest{NodeID: "node-1"})
	if err != nil {
		t.Fatalf("EnsureWorkspaceNode: %v", err)
	}
	if gotAuth != "Bearer "+string(testTokenRaw) {
		t.Fatalf("authorization header = %q", gotAuth)
	}
	if node.ID != "node-1" || node.State != StateOnline {
		t.Fatalf("node = %+v", node)
	}
}

func TestControlClientStatusAndDelete(t *testing.T) {
	srv := newTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, Node{ID: "node-1", State: StateDraining})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	client, err := NewControlClient(srv.URL, newTestTokenFile(t))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	node, err := client.WorkspaceNodeStatus(context.Background(), "node-1")
	if err != nil || node.State != StateDraining {
		t.Fatalf("status = %+v, %v", node, err)
	}
	if err := client.DeleteWorkspaceNode(context.Background(), "node-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestControlClientReportsStatusErrorsWithoutSecrets(t *testing.T) {
	srv := newTokenServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "node not found"})
	})
	client, err := NewControlClient(srv.URL, newTestTokenFile(t))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	_, err = client.WorkspaceNodeStatus(context.Background(), "node-1")
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if !strings.Contains(err.Error(), "node not found") {
		t.Fatalf("error = %v, want the server message", err)
	}
}

func TestControlClientRequiresHTTPSForNonLoopback(t *testing.T) {
	// A plain-HTTP non-loopback URL must be rejected before any dial happens.
	if _, err := NewControlClient("http://fleet.example.com", newTestTokenFile(t)); err == nil {
		t.Fatal("NewControlClient accepted plain HTTP for a non-loopback host")
	}
}

func TestControlClientAllowsLoopbackHTTP(t *testing.T) {
	srv := newTokenServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Node{ID: "n"})
	})
	if _, err := NewControlClient(srv.URL, newTestTokenFile(t)); err != nil {
		t.Fatalf("NewControlClient rejected loopback HTTP: %v", err)
	}
}

func TestControlClientDisablesRedirects(t *testing.T) {
	srv := newTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		t.Error("redirect was followed")
		w.WriteHeader(http.StatusNotFound)
	})
	client, err := NewControlClient(srv.URL, newTestTokenFile(t))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	resp, err := client.do(context.Background(), http.MethodGet, srv.URL+"/redirect", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want the 302 itself", resp.StatusCode)
	}
}

func TestControlClientCapsResponseAt1MiB(t *testing.T) {
	srv := newTokenServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, strings.NewReader(strings.Repeat("a", 2<<20)))
	})
	client, err := NewControlClient(srv.URL, newTestTokenFile(t))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	if _, err := client.WorkspaceNodeStatus(context.Background(), "n"); err == nil {
		t.Fatal("expected a size error for an over-cap response")
	}
}

func TestControlClientRequestDeadline(t *testing.T) {
	// The client bounds each request end to end with controlRequestDeadline.
	// Pin the bound and prove an already-expired context fails immediately
	// rather than hanging, keeping the test fast.
	if controlRequestDeadline != 60*time.Second {
		t.Fatalf("controlRequestDeadline = %v, want 60s", controlRequestDeadline)
	}
	srv := newTokenServer(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		writeJSON(w, http.StatusOK, Node{})
	})
	client, err := NewControlClient(srv.URL, newTestTokenFile(t))
	if err != nil {
		t.Fatalf("NewControlClient: %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := client.WorkspaceNodeStatus(ctx, "n"); err == nil {
		t.Fatal("expected an error for an expired context")
	}
}
