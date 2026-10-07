package aurora_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/cloudruntime"
	"github.com/multica-ai/multica/server/internal/fleet"
)

// TestFleetProvisionerEnsureWire pins the production transport: the provisioner
// sends the strict identity body, the enrollment secret in the exported Fleet
// header, the trusted owner as X-User-ID, and maps the returned node onto the
// provider-neutral FleetNode.
func TestFleetProvisionerEnsureWire(t *testing.T) {
	const (
		owner = "11111111-1111-4111-8111-111111111111"
		node  = "22222222-2222-4222-8222-222222222222"
		key   = "private-key-012345678901234567890123456789"
	)
	req := aurora.FleetEnsureRequest{
		NodeID:          node,
		WorkspaceID:     "55555555-5555-4555-8555-555555555555",
		RuntimeID:       "66666666-6666-4666-8666-666666666666",
		DaemonID:        "77777777-7777-4777-8777-777777777777",
		EnrollmentToken: "mse_" + strings.Repeat("a", 40),
		ImageDigest:     "ghcr.io/eanfs/multica-aurora@sha256:" + strings.Repeat("b", 64),
		Name:            "aurora-sandbox",
		Spec:            "sandbox",
	}
	var gotOwner, gotEnrollment, gotServiceKey string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOwner = r.Header.Get("X-User-ID")
		gotEnrollment = r.Header.Get(fleet.AuroraEnrollmentHeader)
		gotServiceKey = r.Header.Get("X-Fleet-Service-Key")
		if r.Method != http.MethodPut || r.URL.Path != "/internal/v1/workspace-nodes/"+node {
			t.Errorf("route = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"22222222-2222-4222-8222-222222222222","owner_id":"11111111-1111-4111-8111-111111111111","status":"launching","instance_id":"container-9"}`))
	}))
	defer srv.Close()

	p := aurora.NewFleetProvisioner(cloudruntime.NewClient(cloudruntime.Config{BaseURL: srv.URL, ServiceSecret: []byte(key)}))
	got, err := p.EnsureWorkspaceNode(context.Background(), owner, req)
	if err != nil {
		t.Fatalf("ensure workspace node: %v", err)
	}
	if got != (aurora.FleetNode{ID: node, State: "launching", BackendID: "container-9"}) {
		t.Fatalf("fleet node = %+v, want the mapped Fleet response", got)
	}
	if gotOwner != owner {
		t.Fatalf("X-User-ID = %q, want the trusted owner %q", gotOwner, owner)
	}
	if gotEnrollment != req.EnrollmentToken {
		t.Fatalf("enrollment header = %q, want the request secret", gotEnrollment)
	}
	if gotServiceKey != key {
		t.Fatalf("service key header = %q, want the configured key", gotServiceKey)
	}
	if len(body) != 7 {
		t.Fatalf("strict body fields = %d (%v), want 7", len(body), body)
	}
	if _, ok := body["enrollment_token"]; ok {
		t.Fatal("enrollment secret leaked into the JSON body")
	}
	for field, want := range map[string]string{
		"workspace_id": req.WorkspaceID,
		"runtime_id":   req.RuntimeID,
		"daemon_id":    req.DaemonID,
		"image_digest": req.ImageDigest,
		"name":         req.Name,
		"spec":         req.Spec,
	} {
		if body[field] != want {
			t.Fatalf("body[%s] = %q, want %q", field, body[field], want)
		}
	}
	if body["idempotency_key"] == "" {
		t.Fatal("idempotency_key is empty")
	}
}

// TestFleetProvisionerDeleteNotFound pins cleanup idempotency: the client's
// Fleet-not-found error is translated into the aurora-level sentinel the reaper
// treats as success.
func TestFleetProvisionerDeleteNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := aurora.NewFleetProvisioner(cloudruntime.NewClient(cloudruntime.Config{BaseURL: srv.URL, ServiceSecret: []byte(strings.Repeat("k", 32))}))
	err := p.DeleteWorkspaceNode(context.Background(), "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
	if !errors.Is(err, aurora.ErrNodeNotFound) {
		t.Fatalf("delete error = %v, want aurora.ErrNodeNotFound", err)
	}
}
