package aurorafleet

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestController(t *testing.T) (*MemoryBackend, *httptest.Server) {
	t.Helper()
	backend := NewMemoryBackend()
	ctrl := NewController(Config{
		Backend:      backend,
		SandboxImage: "aurora-sandbox:test",
		ServerURL:    "http://multica.test",
		SandboxToken: "test-sandbox-token",
	})
	srv := httptest.NewServer(ctrl.Handler())
	t.Cleanup(srv.Close)
	return backend, srv
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeNode(t *testing.T, resp *http.Response) Node {
	t.Helper()
	var node Node
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		t.Fatalf("decode node: %v", err)
	}
	return node
}

func decodeNodes(t *testing.T, resp *http.Response) []Node {
	t.Helper()
	var envelope struct {
		Nodes []Node `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode nodes: %v", err)
	}
	return envelope.Nodes
}

// TestControllerProvisionLifecycle covers the provision/status/stop/start/reboot/
// terminate arc the cloudruntime surface exposes.
func TestControllerProvisionLifecycle(t *testing.T) {
	_, srv := newTestController(t)

	create := postJSON(t, srv.URL+"/api/v1/nodes", map[string]any{"name": "worker-1"})
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", create.StatusCode)
	}
	node := decodeNode(t, create)
	if node.ID == "" || node.Status != StatusRunning {
		t.Fatalf("created node = %+v, want running with id", node)
	}
	if node.Image != "aurora-sandbox:test" {
		t.Fatalf("created node image = %q, want default sandbox image", node.Image)
	}

	// Status reports the same node.
	status := postJSON(t, srv.URL+"/api/v1/nodes/status", map[string]any{"id": node.ID})
	if status.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", status.StatusCode)
	}
	if got := decodeNode(t, status); got.ID != node.ID {
		t.Fatalf("status node id = %q, want %q", got.ID, node.ID)
	}

	// Stop -> stopped, Start -> running.
	stop := postJSON(t, srv.URL+"/api/v1/nodes/stop", map[string]any{"id": node.ID})
	if got := decodeNode(t, stop); got.Status != StatusStopped {
		t.Fatalf("stop status = %q, want stopped", got.Status)
	}
	start := postJSON(t, srv.URL+"/api/v1/nodes/start", map[string]any{"id": node.ID})
	if got := decodeNode(t, start); got.Status != StatusRunning {
		t.Fatalf("start status = %q, want running", got.Status)
	}

	// Reboot -> rebooting.
	reboot := postJSON(t, srv.URL+"/api/v1/nodes/reboot", map[string]any{"id": node.ID})
	if got := decodeNode(t, reboot); got.Status != StatusRebooting {
		t.Fatalf("reboot status = %q, want rebooting", got.Status)
	}

	// Delete removes it, so a later status is 404.
	raw, err := srv.Client().Do(mustNewRequest(t, http.MethodDelete, srv.URL+"/api/v1/nodes", `{"id":"`+node.ID+`"}`))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer raw.Body.Close()
	if raw.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", raw.StatusCode)
	}

	after := postJSON(t, srv.URL+"/api/v1/nodes/status", map[string]any{"id": node.ID})
	if after.StatusCode != http.StatusNotFound {
		t.Fatalf("status after delete = %d, want 404", after.StatusCode)
	}
}

func mustNewRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

// TestControllerInjectsBootstrapEnv pins the secret the fleet hands a node: the
// server URL and managed-registration token reach the node's environment but
// never the API response.
func TestControllerInjectsBootstrapEnv(t *testing.T) {
	backend, srv := newTestController(t)

	create := postJSON(t, srv.URL+"/api/v1/nodes", map[string]any{})
	raw, err := io.ReadAll(create.Body)
	if err != nil {
		t.Fatalf("read create body: %v", err)
	}
	var node Node
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("decode node: %v", err)
	}

	env := backend.NodeEnv(node.ID)
	if env[EnvServerURL] != "http://multica.test" {
		t.Fatalf("injected server url = %q", env[EnvServerURL])
	}
	if env[EnvSandboxToken] != "test-sandbox-token" {
		t.Fatalf("injected sandbox token = %q", env[EnvSandboxToken])
	}

	// The token is a server-side secret: it must not appear in the node JSON.
	if bytes.Contains(raw, []byte("test-sandbox-token")) {
		t.Fatalf("create response leaked the sandbox token: %s", raw)
	}
}

func TestControllerExec(t *testing.T) {
	_, srv := newTestController(t)

	create := postJSON(t, srv.URL+"/api/v1/nodes", map[string]any{})
	node := decodeNode(t, create)

	resp := postJSON(t, srv.URL+"/api/v1/nodes/exec", map[string]any{
		"id":      node.ID,
		"command": []string{"echo", "hi"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		ExitCode int `json:"exit_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode exec: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exec exit code = %d, want 0", out.ExitCode)
	}
}

func TestControllerListAndReady(t *testing.T) {
	_, srv := newTestController(t)

	postJSON(t, srv.URL+"/api/v1/nodes", map[string]any{"name": "a"})
	postJSON(t, srv.URL+"/api/v1/nodes", map[string]any{"name": "b"})

	list, err := http.Get(srv.URL + "/api/v1/nodes")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer list.Body.Close()
	nodes := decodeNodes(t, list)
	if len(nodes) != 2 {
		t.Fatalf("listed %d nodes, want 2", len(nodes))
	}

	ready, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	defer ready.Body.Close()
	if ready.StatusCode != http.StatusOK {
		t.Fatalf("ready = %d, want 200", ready.StatusCode)
	}
}

func TestControllerUnknownNode(t *testing.T) {
	_, srv := newTestController(t)

	for _, endpoint := range []string{"/status", "/start", "/stop", "/reboot", "/exec"} {
		var body any = map[string]any{"id": "does-not-exist"}
		if endpoint == "/exec" {
			body = map[string]any{"id": "does-not-exist", "command": []string{"echo"}}
		}
		resp := postJSON(t, srv.URL+"/api/v1/nodes"+endpoint, body)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s unknown node = %d, want 404", endpoint, resp.StatusCode)
		}
	}
}
