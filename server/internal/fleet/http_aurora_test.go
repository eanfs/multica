package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	guuid "github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const auroraTestSecret = "test-only-service-secret"

func auroraHTTPFixture(t *testing.T) (*Service, *testutil.Fixture) {
	t.Helper()
	pool, f := testutil.NewFleetFixture(t)
	ns := "http-aurora-" + f.UserID
	f.Cleanup(t, "DELETE FROM fleet_node_operations WHERE namespace=$1", ns)
	f.Cleanup(t, "DELETE FROM fleet_nodes WHERE namespace=$1", ns)
	cfg := model.Config{Namespace: ns, Image: "aurora-test-image", Specs: map[string]model.Spec{"sandbox": {CPUs: 2, MemoryBytes: 4294967296, Pids: 256, MaxRuns: 1}}, Aurora: &model.AuroraConfig{ServerURL: "http://api.test"}}
	repo := store.New(pool, ns, store.WithProvisioningConfig(cfg), store.WithMaxNodes(2))
	return NewService(repo, cfg, nil), f
}

func auroraBody(ws, rt, daemon, image, name, key string) string {
	return fmt.Sprintf("{\"workspace_id\":%q,\"runtime_id\":%q,\"daemon_id\":%q,\"image_digest\":%q,\"name\":%q,\"spec\":\"sandbox\",\"idempotency_key\":%q}", ws, rt, daemon, image, name, key)
}

func auroraPut(t *testing.T, h http.Handler, owner, nodeID, token, body string) *testutil.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/internal/v1/workspace-nodes/"+nodeID, strings.NewReader(body))
	req.Header.Set("X-Fleet-Service-Key", auroraTestSecret)
	req.Header.Set("X-User-ID", owner)
	if token != "" {
		req.Header.Set(AuroraEnrollmentHeader, token)
	}
	return testutil.Call(t, h.ServeHTTP, req)
}

func TestHTTPAuroraWorkspaceNodeRoutes(t *testing.T) {
	svc, f := auroraHTTPFixture(t)
	h := svc.Handler([]byte(auroraTestSecret))
	nodeID := guuid.NewString()
	rt, daemon := guuid.NewString(), guuid.NewString()
	token := "mse_" + strings.Repeat("a", 40)
	body := auroraBody(f.WorkspaceID, rt, daemon, "aurora-test-image", "aurora", "key-1")

	w := auroraPut(t, h, f.UserID, nodeID, token, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("put=%d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil || out["id"] != nodeID || out["status"] != "launching" || out["operation_id"] == "" {
		t.Fatalf("accepted=%v err=%v", out, e)
	}
	if strings.Contains(w.Body.String(), "mse_") || strings.Contains(w.Body.String(), token) {
		t.Fatal("enrollment secret leaked into the response")
	}

	// The one-time secret is handed to the reconciler exactly once.
	if got, ok := svc.TakeAuroraEnrollment(mustUUID(t, nodeID)); !ok || got != token {
		t.Fatalf("handoff = %q %v", got, ok)
	}
	if _, ok := svc.TakeAuroraEnrollment(mustUUID(t, nodeID)); ok {
		t.Fatal("handoff secret was reusable")
	}

	// A replay of the same key/identity returns the same operation.
	if replay := auroraPut(t, h, f.UserID, nodeID, token, body); replay.Code != http.StatusAccepted || replay.Body.String() != w.Body.String() {
		t.Fatalf("replay=%d %s", replay.Code, replay.Body.String())
	}
	// A different payload under the same key conflicts.
	if conflict := auroraPut(t, h, f.UserID, nodeID, token, auroraBody(f.WorkspaceID, rt, daemon, "aurora-test-image", "other", "key-1")); conflict.Code != http.StatusConflict {
		t.Fatalf("conflict=%d %s", conflict.Code, conflict.Body.String())
	}

	// Strict body and secret handling.
	for name, tc := range map[string]struct {
		token string
		body  string
	}{
		"missing-secret":   {"", body},
		"malformed-secret": {"mse_short", body},
		"unknown-field":    {token, strings.TrimSuffix(body, "}") + ",\"image\":\"evil\"}"},
		"secret-in-body":   {token, strings.TrimSuffix(body, "}") + ",\"enrollment_token\":\"" + token + "\"}"},
		"missing-identity": {token, "{}"},
	} {
		if got := auroraPut(t, h, f.UserID, nodeID, tc.token, tc.body); got.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", name, got.Code, got.Body.String())
		}
	}

	// Auth and ownership.
	noKey := httptest.NewRequest(http.MethodPut, "/internal/v1/workspace-nodes/"+nodeID, strings.NewReader(body))
	noKey.Header.Set("X-User-ID", f.UserID)
	noKey.Header.Set(AuroraEnrollmentHeader, token)
	if got := testutil.Call(t, h.ServeHTTP, noKey); got.Code != http.StatusUnauthorized {
		t.Fatalf("no-key status=%d", got.Code)
	}
	if got := auroraPut(t, h, "", nodeID, token, body); got.Code != http.StatusUnauthorized {
		t.Fatalf("no-owner status=%d", got.Code)
	}
	if got := call(t, h, "GET", "/internal/v1/workspace-nodes/not-a-uuid", f.UserID, ""); got.Code != http.StatusBadRequest {
		t.Fatalf("bad-node status=%d", got.Code)
	}

	// Read and delete are owner-scoped.
	got := call(t, h, "GET", "/internal/v1/workspace-nodes/"+nodeID, f.UserID, "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), nodeID) {
		t.Fatalf("get=%d %s", got.Code, got.Body.String())
	}
	other := f.User(t, "aurora-http-other", "aurora-http-other-"+f.UserID+"@test.invalid")
	if got = call(t, h, "GET", "/internal/v1/workspace-nodes/"+nodeID, other, ""); got.Code != http.StatusForbidden {
		t.Fatalf("cross-owner get=%d", got.Code)
	}
	if got = call(t, h, "DELETE", "/internal/v1/workspace-nodes/"+nodeID, f.UserID, ""); got.Code != http.StatusNoContent {
		t.Fatalf("delete=%d %s", got.Code, got.Body.String())
	}
	if got = call(t, h, "DELETE", "/internal/v1/workspace-nodes/"+nodeID, f.UserID, ""); got.Code != http.StatusNoContent {
		t.Fatalf("idempotent delete=%d", got.Code)
	}
}

func TestHTTPAuroraRoutesAbsentWithoutProfile(t *testing.T) {
	h := NewService(nil, model.Config{Namespace: "no-aurora"}, nil).Handler([]byte(auroraTestSecret))
	nodeID := guuid.NewString()
	if got := call(t, h, "GET", "/internal/v1/workspace-nodes/"+nodeID, "11111111-1111-4111-8111-111111111111", ""); got.Code != http.StatusNotFound {
		t.Fatalf("get without profile=%d", got.Code)
	}
	if got := call(t, h, "DELETE", "/internal/v1/workspace-nodes/"+nodeID, "11111111-1111-4111-8111-111111111111", ""); got.Code != http.StatusNotFound {
		t.Fatalf("delete without profile=%d", got.Code)
	}
	if got := auroraPut(t, h, "11111111-1111-4111-8111-111111111111", nodeID, "mse_"+strings.Repeat("a", 40), "{}"); got.Code != http.StatusNotFound {
		t.Fatalf("put without profile=%d", got.Code)
	}
}
