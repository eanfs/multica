package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/fleet"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cloudruntime"
)

// Billing must not reuse the node client: local Fleet has no billing authority.
func TestLocalFleetBillingIsolated(t *testing.T) {
	useCloudRuntimeProxy(t, &fakeCloudRuntimeProxy{enabled: true, resp: &cloudruntime.Response{StatusCode: 200, Body: []byte("{}")}})
	w := httptest.NewRecorder()
	testHandler.GetCloudBillingBalance(w, newRequest("GET", "/api/cloud-billing/balance", nil))
	if w.Code != 403 {
		t.Fatalf("billing used node client: %d %s", w.Code, w.Body.String())
	}
}

type fakeCloudRuntimeProxy struct {
	enabled bool
	req     cloudruntime.Request
	resp    *cloudruntime.Response
	err     error
	called  bool
}

func (f *fakeCloudRuntimeProxy) Enabled() bool {
	return f.enabled
}

func (f *fakeCloudRuntimeProxy) Do(ctx context.Context, req cloudruntime.Request) (*cloudruntime.Response, error) {
	f.called = true
	f.req = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func useCloudRuntimeProxy(t *testing.T, proxy cloudRuntimeProxy) {
	t.Helper()

	prevProxy := testHandler.CloudRuntime
	testHandler.CloudRuntime = proxy
	t.Cleanup(func() { testHandler.CloudRuntime = prevProxy })
}

// TestCreateCloudRuntimeNodeForwardsBody is the post-MUL-2671 happy
// path for CreateCloudRuntimeNode: the handler no longer reads, asks
// for, or auto-generates an mul_ PAT — Cloud now mints its own
// node-scoped mcn_ PAT during /api/v1/nodes and ships it to the EC2
// instance via SSM. Multica-api just forwards the request body and
// the caller's user_id; there is no PAT plumbing on this endpoint.
func TestCreateCloudRuntimeNodeForwardsBody(t *testing.T) {
	proxy := &fakeCloudRuntimeProxy{
		enabled: true,
		resp: &cloudruntime.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"X-Request-Id": []string{"fleet-request-id"}},
			Body:       []byte(`{"status":"launching"}`),
		},
	}
	useCloudRuntimeProxy(t, proxy)

	req := newRequest(http.MethodPost, "/api/cloud-runtime/nodes", map[string]any{
		"instance_type": "g5.xlarge",
	})
	req.Header.Set("X-Request-ID", "api-request-id")
	w := httptest.NewRecorder()

	testHandler.CreateCloudRuntimeNode(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !proxy.called {
		t.Fatal("cloud runtime proxy was not called")
	}
	if proxy.req.Method != http.MethodPost || proxy.req.Path != "/api/v1/nodes" {
		t.Fatalf("proxied request = %s %s", proxy.req.Method, proxy.req.Path)
	}
	if proxy.req.UserID != testUserID {
		t.Fatalf("proxied user id = %q", proxy.req.UserID)
	}
	if proxy.req.RequestID != "api-request-id" {
		t.Fatalf("proxied request id = %q", proxy.req.RequestID)
	}
	if got := w.Header().Get("X-Request-ID"); got != "fleet-request-id" {
		t.Fatalf("response request id = %q", got)
	}
}

func TestCloudRuntimeDisabledReturnsForbidden(t *testing.T) {
	useCloudRuntimeProxy(t, &fakeCloudRuntimeProxy{enabled: false})

	req := newRequest(http.MethodGet, "/api/cloud-runtime/nodes", nil)
	w := httptest.NewRecorder()

	testHandler.ListCloudRuntimeNodes(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["code"] != "cloud_runtime_not_configured" {
		t.Fatalf("unexpected disabled response: body=%v err=%v", body, err)
	}
}

func TestListCloudRuntimeNodesForwardsQuery(t *testing.T) {
	proxy := &fakeCloudRuntimeProxy{
		enabled: true,
		resp: &cloudruntime.Response{
			StatusCode: http.StatusOK,
			Body:       []byte(`[]`),
		},
	}
	useCloudRuntimeProxy(t, proxy)

	req := newRequest(http.MethodGet, "/api/cloud-runtime/nodes?limit=10&offset=20", nil)
	w := httptest.NewRecorder()

	testHandler.ListCloudRuntimeNodes(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !proxy.called {
		t.Fatal("cloud runtime proxy was not called")
	}
	if proxy.req.Method != http.MethodGet || proxy.req.Path != "/api/v1/nodes" {
		t.Fatalf("proxied request = %s %s", proxy.req.Method, proxy.req.Path)
	}
	if got := proxy.req.Query.Encode(); got != "limit=10&offset=20" {
		t.Fatalf("proxied query = %q", got)
	}
}

func TestCloudRuntimeNonJSONResponseIsWrapped(t *testing.T) {
	proxy := &fakeCloudRuntimeProxy{
		enabled: true,
		resp: &cloudruntime.Response{
			StatusCode: http.StatusBadGateway,
			Body:       []byte("fleet failed\n"),
		},
	}
	useCloudRuntimeProxy(t, proxy)

	req := newRequest(http.MethodGet, "/api/cloud-runtime/healthz", nil)
	w := httptest.NewRecorder()

	testHandler.GetCloudRuntimeHealth(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	if got := w.Body.String(); !strings.Contains(got, `"error":"fleet failed"`) {
		t.Fatalf("body = %s", got)
	}
}

func TestCloudRuntimeEmptyResponseKeepsStatus(t *testing.T) {
	proxy := &fakeCloudRuntimeProxy{
		enabled: true,
		resp: &cloudruntime.Response{
			StatusCode: http.StatusNoContent,
			Body:       nil,
		},
	}
	useCloudRuntimeProxy(t, proxy)

	req := newRequest(http.MethodGet, "/api/cloud-runtime/healthz", nil)
	w := httptest.NewRecorder()

	testHandler.GetCloudRuntimeHealth(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != "" {
		t.Fatalf("body = %s", body)
	}
}

func TestCreateCloudRuntimeNodeRejectsLargeBody(t *testing.T) {
	proxy := &fakeCloudRuntimeProxy{
		enabled: true,
		resp: &cloudruntime.Response{
			StatusCode: http.StatusCreated,
			Body:       []byte(`{"status":"launching"}`),
		},
	}
	useCloudRuntimeProxy(t, proxy)

	body := bytes.NewReader(bytes.Repeat([]byte("a"), maxCloudRuntimeRequestBodySize+1))
	req := httptest.NewRequest(http.MethodPost, "/api/cloud-runtime/nodes", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", testUserID)
	w := httptest.NewRecorder()

	testHandler.CreateCloudRuntimeNode(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if proxy.called {
		t.Fatal("cloud runtime proxy should not be called")
	}
}

// Dropping the mutation header creates non-idempotent intents; accepting invalid keys must fail locally.
func TestLocalFleetMutationHeaders(t *testing.T) {
	secret := []byte("test-only-012345678901234567890123456789")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "once" || r.Header.Get("X-User-ID") != testUserID || r.Header.Get("X-Fleet-Service-Key") != string(secret) || r.Header.Get("Authorization") != "" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(202)
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	h := *testHandler
	h.cfg.LocalFleetURL = srv.URL
	h.CloudRuntime = cloudruntime.NewClient(cloudruntime.Config{BaseURL: srv.URL, ServiceSecret: secret})
	for _, tc := range []struct {
		name, method, body string
		fn                 http.HandlerFunc
	}{
		{"create", "POST", `{"instance_type":"small","name":"node"}`, h.CreateCloudRuntimeNode},
		{"start", "POST", `{"instance_id":"node"}`, h.StartCloudRuntimeNode},
		{"stop", "POST", `{"instance_id":"node"}`, h.StopCloudRuntimeNode},
		{"reboot", "POST", `{"instance_id":"node"}`, h.RebootCloudRuntimeNode},
		{"delete", "DELETE", `{"instance_id":"node"}`, h.DeleteCloudRuntimeNode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, keys := range [][]string{nil, {""}, {"a", "b"}, {strings.Repeat("x", 129)}, {"once"}} {
				req := newRequest(tc.method, "/api/cloud-runtime/nodes", nil)
				req.Body = io.NopCloser(strings.NewReader(tc.body))
				req.Header["Idempotency-Key"] = keys
				req.Header.Set("X-Fleet-Service-Key", "forged")
				req.Header.Set("Authorization", "Bearer fake")
				w := httptest.NewRecorder()
				tc.fn(w, req)
				want := 400
				if len(keys) == 1 && keys[0] == "once" && tc.name == "create" {
					want = 202
				} // Real valid lifecycle acceptance is owned by the full-hop test.
				if w.Code != want {
					t.Fatalf("keys=%v status=%d want=%d body=%s", keys, w.Code, want, w.Body.String())
				}
			}
		})
	}
}

// Task7 extends this canonical test with real lifecycle producers, not synthetic operations.
func TestCloudRuntimeIdempotencyFullHop(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task5-fullhop-" + f.UserID
	t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
	cleanupTask5Fleet(t, pool, ns, f.UserID, f.WorkspaceID)
	f.FleetProfile(t, ns)
	cfg := model.Config{Namespace: ns, Image: "fake-approved-image", MaxNodes: 2, Specs: map[string]model.Spec{"small": {CPUs: 2, MemoryBytes: 4294967296, Pids: 256, MaxRuns: 1}}}
	repo := store.New(pool, ns, store.WithProvisioningConfig(cfg))
	secret := []byte("test-only-012345678901234567890123456789")
	upstream := httptest.NewServer(fleet.NewService(repo, cfg, task7DiagnosticProvider{}).Handler(secret))
	defer upstream.Close()
	h := *testHandler
	h.Queries = db.New(pool)
	h.DB = pool
	h.TxStarter = pool
	h.cfg.LocalFleetURL = upstream.URL
	h.CloudRuntime = cloudruntime.NewClient(cloudruntime.Config{BaseURL: upstream.URL, ServiceSecret: secret})
	router := chi.NewRouter()
	router.Use(middleware.Auth(h.Queries, nil, nil, nil))
	router.Use(middleware.RequireWorkspaceMember(h.Queries))
	router.Post("/api/cloud-runtime/nodes", h.CreateCloudRuntimeNode)
	router.Post("/api/cloud-runtime/nodes/start", h.StartCloudRuntimeNode)
	router.Post("/api/cloud-runtime/nodes/stop", h.StopCloudRuntimeNode)
	router.Post("/api/cloud-runtime/nodes/reboot", h.RebootCloudRuntimeNode)
	router.Post("/api/cloud-runtime/nodes/delete", h.DeleteCloudRuntimeNode)
	router.Post("/api/tasks/{taskId}/cancel", h.CancelTaskByUser)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": f.UserID, "exp": time.Now().Add(time.Hour).Unix()}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/cloud-runtime/nodes", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Workspace-ID", f.WorkspaceID)
		req.Header.Set("Idempotency-Key", "fullhop-once")
		req.Header.Set("X-User-ID", "11111111-1111-4111-8111-111111111111")
		req.Header.Set("X-Fleet-Service-Key", "forged")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	first := call(`{"instance_type":"small","name":"node"}`)
	if first.Code != 202 {
		t.Fatalf("create=%d %s", first.Code, first.Body.String())
	}
	var one map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if id, ok := one["id"].(string); !ok || id == "" {
		t.Fatalf("missing node ID: %v", one)
	}
	if op, ok := one["operation_id"].(string); !ok || op == "" {
		t.Fatalf("missing operation ID: %v", one)
	}
	replay := call(`{"instance_type":"small","name":"node"}`)
	var two map[string]any
	_ = json.Unmarshal(replay.Body.Bytes(), &two)
	if replay.Code != 202 || one["id"] != two["id"] || one["operation_id"] != two["operation_id"] || one["owner_id"] != f.UserID {
		t.Fatalf("replay changed identity: %v / %v", one, two)
	}
	if w := call(`{"instance_type":"small","name":"changed"}`); w.Code != 409 {
		t.Fatalf("conflict=%d %s", w.Code, w.Body.String())
	}
	if w := call(`{"instance_type":"small","name":"node","image_id":"override"}`); w.Code != 400 {
		t.Fatalf("override=%d %s", w.Code, w.Body.String())
	}
	ctx := context.Background()
	alternate := f.FleetNode(t, ns, testutil.Cols{"status": "stopped", "desired": "stopped"})
	for _, action := range []string{"start", "stop", "reboot", "delete"} {
		if _, e := pool.Exec(ctx, "UPDATE fleet_node_operations SET phase='completed' WHERE namespace=$1 AND node_id=$2", ns, one["id"]); e != nil {
			t.Fatal(e)
		}
		status := "running"
		desired := "running"
		if action == "start" {
			status = "stopped"
			desired = "stopped"
		}
		if _, e := pool.Exec(ctx, "UPDATE fleet_nodes SET status=$3,desired=$4,maintenance=false,container_id='fake-container',start_epoch='current-epoch',data_volume='owned-data',ready=false WHERE namespace=$1 AND id=$2", ns, one["id"], status, desired); e != nil {
			t.Fatal(e)
		}
		invoke := func(key string, target any) *httptest.ResponseRecorder {
			raw, _ := json.Marshal(map[string]any{"instance_id": target})
			r := httptest.NewRequest("POST", "/api/cloud-runtime/nodes/"+action, bytes.NewReader(raw))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Workspace-ID", f.WorkspaceID)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", key)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			return w
		}
		if action == "delete" {
			rt := f.Runtime(t, "managed", testutil.Cols{"metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, one["id"]))})
			agent := f.Agent(t, "retained", rt)
			issue := f.Issue(t, "retained")
			task := f.Task(t, agent, testutil.Cols{"runtime_id": rt, "issue_id": issue, "status": "queued"})
			if refused := invoke("fullhop-"+action, one["id"]); refused.Code != 409 {
				t.Fatalf("queued delete=%d %s", refused.Code, refused.Body.String())
			}
			persisted, e := h.Queries.GetAgentTask(ctx, util.MustParseUUID(task))
			if e != nil || persisted.Status != "queued" {
				t.Fatal("Fleet silently cancelled queued business work", e)
			}
			cancellation := httptest.NewRequest("POST", "/api/tasks/"+task+"/cancel", nil)
			cancellation.Header.Set("Authorization", "Bearer "+token)
			cancellation.Header.Set("X-Workspace-ID", f.WorkspaceID)
			cw := httptest.NewRecorder()
			router.ServeHTTP(cw, cancellation)
			if cw.Code != 200 {
				t.Fatalf("existing business cancellation=%d %s", cw.Code, cw.Body.String())
			}
			persisted, e = h.Queries.GetAgentTask(ctx, util.MustParseUUID(task))
			if e != nil || persisted.Status != "cancelled" {
				t.Fatal("business cancellation not persisted", e)
			}
		}
		a := invoke("fullhop-"+action, one["id"])
		if a.Code != 202 {
			t.Fatalf("actual %s=%d %s", action, a.Code, a.Body.String())
		}
		replay := invoke("fullhop-"+action, one["id"])
		if replay.Code != 202 || replay.Body.String() != a.Body.String() {
			t.Fatalf("fullhop %s replay=%d %s", action, replay.Code, replay.Body.String())
		}
		if conflicting := invoke("fullhop-"+action, alternate); conflicting.Code != 409 {
			t.Fatalf("%s changed payload=%d %s", action, conflicting.Code, conflicting.Body.String())
		}
		var operation map[string]any
		if e := json.Unmarshal(a.Body.Bytes(), &operation); e != nil {
			t.Fatal(e)
		}
		if operation["id"] != one["id"] || operation["operation_id"] == "" {
			t.Fatalf("lifecycle DTO=%s", a.Body.String())
		}
	}

}
