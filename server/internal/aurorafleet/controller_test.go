package aurorafleet

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture IDs in UUID form, matching the shape plan A persists.
const (
	testNodeID      = "01933e5f-8a2c-7d4e-9f01-2a3b4c5d6e7f"
	testWorkspaceID = "01933e5f-8a2c-7d4e-9f01-2a3b4c5d6e80"
	testRuntimeID   = "01933e5f-8a2c-7d4e-9f01-2a3b4c5d6e81"
	testDaemonID    = "01933e5f-8a2c-7d4e-9f01-2a3b4c5d6e82"
	testEnrollToken = "mse_0123456789abcdef0123456789abcdef01234567"
)

// newTestController builds an authenticated controller over an in-memory
// backend with a fresh secret root. It returns the backend, the secret root,
// the control bearer token, and the server.
func newTestController(t *testing.T) (*MemoryBackend, string, string, *httptest.Server) {
	t.Helper()
	backend := NewMemoryBackend()
	secretRoot := t.TempDir()
	tokenPath := writeTokenFile(t, t.TempDir(), testTokenB64, 0o400)
	auth, err := LoadControlAuth(tokenPath)
	if err != nil {
		t.Fatalf("LoadControlAuth: %v", err)
	}
	ctrl := NewController(Config{Backend: backend, Auth: auth, SecretRoot: secretRoot})
	srv := httptest.NewServer(ctrl.Handler())
	t.Cleanup(srv.Close)
	return backend, secretRoot, string(testTokenRaw), srv
}

// fleetReq performs an authenticated request against the test server.
func fleetReq(t *testing.T, srv *httptest.Server, method, path, body, bearer string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// validEnsureBody is the exact five-field ensure request the API accepts.
func validEnsureBody(nodeID string) string {
	return `{"node_id":"` + nodeID + `","workspace_id":"` + testWorkspaceID +
		`","runtime_id":"` + testRuntimeID + `","daemon_id":"` + testDaemonID +
		`","enrollment_token":"` + testEnrollToken + `"}`
}

func TestWorkspaceNodeEnsureRequiresControlBearer(t *testing.T) {
	_, _, token, srv := newTestController(t)

	// /healthz is excluded from auth.
	if resp := fleetReq(t, srv, http.MethodGet, "/healthz", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz without bearer = %d, want 200", resp.StatusCode)
	}

	// /readyz and the node route require auth; missing and wrong are identical 401s.
	var missingBody, wrongBody string
	for _, bearer := range []string{"", "wrong-token"} {
		resp := fleetReq(t, srv, http.MethodGet, "/readyz", "", bearer)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("readyz bearer=%q = %d, want 401", bearer, resp.StatusCode)
		}
		respEnsure := fleetReq(t, srv, http.MethodPut, "/internal/v1/workspace-nodes/"+testNodeID, validEnsureBody(testNodeID), bearer)
		if respEnsure.StatusCode != http.StatusUnauthorized {
			t.Fatalf("ensure bearer=%q = %d, want 401", bearer, respEnsure.StatusCode)
		}
		rawEnsure, _ := io.ReadAll(respEnsure.Body)
		if !strings.Contains(string(rawEnsure), "unauthorized") {
			t.Fatalf("ensure 401 body = %q", rawEnsure)
		}
		if bearer == "" {
			missingBody = string(rawEnsure)
		} else {
			wrongBody = string(rawEnsure)
		}
	}
	if missingBody != wrongBody {
		t.Fatalf("missing and wrong bearer 401 bodies differ: %q vs %q", missingBody, wrongBody)
	}

	// With the correct bearer the ensure proceeds.
	resp := fleetReq(t, srv, http.MethodPut, "/internal/v1/workspace-nodes/"+testNodeID, validEnsureBody(testNodeID), token)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("ensure = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
}

func TestWorkspaceNodeEnsureRejectsIdentityMismatch(t *testing.T) {
	_, _, token, srv := newTestController(t)

	other := "01933e5f-8a2c-7d4e-9f01-2a3b4c5d6eff"
	resp := fleetReq(t, srv, http.MethodPut, "/internal/v1/workspace-nodes/"+testNodeID, validEnsureBody(other), token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("path/body node mismatch = %d, want 400", resp.StatusCode)
	}

	// Malformed JSON is also a 400.
	resp = fleetReq(t, srv, http.MethodPut, "/internal/v1/workspace-nodes/"+testNodeID, "{not json", token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON = %d, want 400", resp.StatusCode)
	}

	// Malformed or missing UUID / token formats are rejected.
	bad := map[string]string{
		"node_id":          "not-a-uuid",
		"workspace_id":     "also-not",
		"runtime_id":       "",
		"daemon_id":        "bad",
		"enrollment_token": "mdt_0123456789abcdef0123456789abcdef01234567",
	}
	for field, value := range bad {
		var m map[string]any
		if err := json.Unmarshal([]byte(validEnsureBody(testNodeID)), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if value == "" {
			delete(m, field)
		} else {
			m[field] = value
		}
		raw, _ := json.Marshal(m)
		resp := fleetReq(t, srv, http.MethodPut, "/internal/v1/workspace-nodes/"+testNodeID, string(raw), token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad %s = %d, want 400", field, resp.StatusCode)
		}
	}
}

func TestWorkspaceNodeEnsureDoesNotAcceptImageOrCommand(t *testing.T) {
	_, _, token, srv := newTestController(t)

	for _, extra := range []map[string]any{
		{"image": "evil:latest"},
		{"command": []string{"sh", "-c"}},
		{"env": map[string]string{"X": "y"}},
		{"labels": map[string]string{"a": "b"}},
		{"mounts": []string{"/:/host"}},
		{"cpu_limit": 8},
	} {
		var m map[string]any
		if err := json.Unmarshal([]byte(validEnsureBody(testNodeID)), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for k, v := range extra {
			m[k] = v
		}
		raw, _ := json.Marshal(m)
		resp := fleetReq(t, srv, http.MethodPut, "/internal/v1/workspace-nodes/"+testNodeID, string(raw), token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("extra field %v = %d, want 400", extra, resp.StatusCode)
		}
	}
}

func TestWorkspaceNodeRoutesCallTypedBackend(t *testing.T) {
	backend, secretRoot, token, srv := newTestController(t)

	// Ensure: the backend receives the typed spec and the enrollment secret is
	// staged through a 0700/0400 file, then removed after the response.
	resp := fleetReq(t, srv, http.MethodPut, "/internal/v1/workspace-nodes/"+testNodeID, validEnsureBody(testNodeID), token)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("ensure = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	var node Node
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		t.Fatalf("decode node: %v", err)
	}
	if node.ID != testNodeID || node.State != StateOnline {
		t.Fatalf("node = %+v", node)
	}

	spec := backend.LastSpec()
	if spec.NodeID != testNodeID || spec.WorkspaceID != testWorkspaceID ||
		spec.RuntimeID != testRuntimeID || spec.DaemonID != testDaemonID {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.EnrollmentFile == "" {
		t.Fatal("backend received no enrollment file path")
	}
	if _, err := os.Stat(spec.EnrollmentFile); !os.IsNotExist(err) {
		t.Fatalf("enrollment file still present after ensure: %v", err)
	}
	if !strings.HasPrefix(spec.EnrollmentFile, filepath.Join(secretRoot, testNodeID)+string(os.PathSeparator)) {
		t.Fatalf("enrollment file %q is outside the node secret dir", spec.EnrollmentFile)
	}

	// The staged file, while it existed, had the required modes. Re-stage it
	// through the controller's own helper to pin parent 0700 / file 0400.
	if _, err := writeEnrollmentSecret(secretRoot, testNodeID, testEnrollToken); err != nil {
		t.Fatalf("re-stage enrollment secret: %v", err)
	}
	info, err := os.Stat(filepath.Join(secretRoot, testNodeID))
	if err != nil {
		t.Fatalf("stat secret dir: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("secret dir mode = %v, want 0700", info.Mode().Perm())
	}
	info, err = os.Stat(spec.EnrollmentFile)
	if err != nil {
		t.Fatalf("stat enrollment file: %v", err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Fatalf("enrollment file mode = %v, want 0400", info.Mode().Perm())
	}
	staged, err := os.ReadFile(spec.EnrollmentFile)
	if err != nil || string(staged) != testEnrollToken {
		t.Fatalf("staged secret = %q, %v", staged, err)
	}
	// Clean up the re-staged copy so delete's cleanup is observed on its own.
	_ = os.Remove(spec.EnrollmentFile)

	// Status returns the node.
	resp = fleetReq(t, srv, http.MethodGet, "/internal/v1/workspace-nodes/"+testNodeID, "", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil || node.ID != testNodeID {
		t.Fatalf("status node = %+v, %v", node, err)
	}

	// Delete removes it; a later status is 404.
	resp = fleetReq(t, srv, http.MethodDelete, "/internal/v1/workspace-nodes/"+testNodeID, "", token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
	resp = fleetReq(t, srv, http.MethodGet, "/internal/v1/workspace-nodes/"+testNodeID, "", token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status after delete = %d, want 404", resp.StatusCode)
	}
}

func TestLegacyFleetExecRouteIsNotExposed(t *testing.T) {
	_, _, token, srv := newTestController(t)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/nodes/exec", `{"id":"x","command":["echo"]}`},
		{http.MethodPost, "/api/v1/nodes", `{"image":"custom:image"}`},
		{http.MethodGet, "/api/v1/nodes", ""},
		{http.MethodPost, "/api/v1/nodes/status", `{"id":"x"}`},
	} {
		resp := fleetReq(t, srv, tc.method, tc.path, tc.body, token)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
	}
}
