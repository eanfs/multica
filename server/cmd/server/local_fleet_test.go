package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/go-redis/redismock/v9"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/fleet"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/seatcapacity"
	"github.com/multica-ai/multica/server/internal/testutil"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Ignoring local mode disables node access instead of isolating SaaS policies.
func TestLocalFleetRouterAssembly(t *testing.T) {
	t.Setenv("MULTICA_CLOUD_URL", "")
	t.Setenv("MULTICA_LOCAL_FLEET_URL", "http://127.0.0.1:19001")
	path := filepath.Join(t.TempDir(), "service-key")
	if err := os.WriteFile(path, []byte("test-only-012345678901234567890123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULTICA_LOCAL_FLEET_SECRET_FILE", path)
	_, h := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	if !h.CloudRuntime.Enabled() {
		t.Fatal("local node client is disabled")
	}
	if h.Entitlements != nil || seatcapacity.CanRunWorker(h.SeatCapacity) || h.SeatCapacityWorker != nil {
		t.Fatal("local mode enabled SaaS policy")
	}
}

func TestLocalFleetRejectsMixedCloud(t *testing.T) {
	for _, path := range []string{"/missing-private-marker", "relative-private-marker", ""} {
		_, err := resolveLocalFleet("https://cloud.example", "http://127.0.0.1:19001", path)
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") || (path != "" && strings.Contains(err.Error(), path)) {
			t.Fatalf("mixed mode must reject before secret access without exposing path: %v", err)
		}
	}
}

func TestLocalFleetRejectsNonregularSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("test-only-012345678901234567890123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveLocalFleet("", "http://127.0.0.1:19001", link); err == nil {
		t.Fatal("nonregular secret reference accepted")
	}
}

func TestLocalFleetConfigSafety(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("test-only-012345678901234567890123456789\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"https://external.invalid", "http://secret@127.0.0.1", "http://127.0.0.1/path", "http://127.0.0.1?secret=value", "file:///private-marker"} {
		_, err := resolveLocalFleet("", u, path)
		if err == nil || strings.Contains(err.Error(), "secret@") || strings.Contains(err.Error(), "private-marker") {
			t.Fatalf("unsafe URL %q: %v", u, err)
		}
	}
	for _, mode := range []os.FileMode{0644, 0620, 0000} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveLocalFleet("", "http://127.0.0.1:19001", path); err == nil {
			t.Fatalf("unsafe file mode %o accepted", mode)
		}
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"http://127.0.0.1:19001/", "https://localhost:19001", "http://[::1]:19001"} {
		cfg, err := resolveLocalFleet("", u, path)
		if err != nil || !cfg.Enabled || string(cfg.Secret) != "test-only-012345678901234567890123456789" {
			t.Fatalf("private local config=%v err=%v", cfg.Enabled, err)
		}
	}
	if _, err := resolveLocalFleet("", "", "missing"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveLocalFleet("", "http://127.0.0.1:19001", path); err == nil {
		t.Fatal("short secret accepted")
	}
}

// A valid private key must not be selected relative to the process working directory.
func TestLocalFleetConfigRejectsRelativeSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relative-private-marker")
	if err := os.WriteFile(path, []byte(strings.Repeat("k", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{relative, " \t" + relative + "\n", "missing-relative-private-marker"} {
		cfg, err := resolveLocalFleet("", "http://127.0.0.1:19001", ref)
		if err == nil || cfg.Enabled || len(cfg.Secret) != 0 {
			t.Fatal("relative private key reference accepted")
		}
		if !strings.Contains(err.Error(), "absolute") || strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), cwd) {
			t.Fatalf("relative path must fail before file access with a path-free absolute-reference error: %v", err)
		}
	}
}

// The byte budget applies to the complete raw file, not a trimmed or truncated prefix.
func TestLocalFleetConfigSecretByteBudget(t *testing.T) {
	key := strings.Repeat("k", 32)
	for _, tc := range []struct {
		name, raw, want string
		reject          bool
	}{
		{name: "minimum", raw: key, want: key},
		{name: "LF", raw: key + "\n", want: key},
		{name: "CRLF", raw: key + "\r\n", want: key},
		{name: "exact_raw_boundary", raw: strings.Repeat("k", 65536), want: strings.Repeat("k", 65536)},
		{name: "padded_raw_boundary", raw: key + strings.Repeat(" ", 65504), want: key},
		{name: "sentinel_whitespace", raw: key + strings.Repeat(" ", 65505), reject: true},
		{name: "oversized_whitespace", raw: key + strings.Repeat(" ", 65506), reject: true},
		{name: "hidden_second_line", raw: key + strings.Repeat(" ", 65505) + "\nhidden-private-marker", reject: true},
		{name: "visible_second_line", raw: key + "\nsecond-private-marker", reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "budget-private-marker")
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := resolveLocalFleet("", "http://127.0.0.1:19001", " \t"+path+"\n")
			if tc.reject {
				if err == nil || cfg.Enabled || len(cfg.Secret) != 0 {
					t.Fatalf("invalid raw key file accepted (%d bytes)", len(tc.raw))
				}
				if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "private-marker") {
					t.Fatal("key validation error exposes private file data")
				}
			} else if err != nil || !cfg.Enabled || string(cfg.Secret) != tc.want {
				t.Fatalf("valid raw key file rejected (%d bytes): %v", len(tc.raw), err)
			}
		})
	}
}

func TestLocalFleetConfigDisabledDoesNotReadSecret(t *testing.T) {
	for _, cloudURL := range []string{"", "https://cloud.example"} {
		for _, path := range []string{"missing-relative-private-marker", t.TempDir()} {
			cfg, err := resolveLocalFleet(cloudURL, " \t", path)
			if err != nil || cfg.Enabled || cfg.URL != "" || len(cfg.Secret) != 0 {
				t.Fatalf("disabled local mode must ignore key reference: %v", err)
			}
		}
	}
}

// The complete production router must map public create through real Fleet admission.
func TestLocalFleetRouterCreateFullHop(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task5-router-" + f.UserID
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, table := range []string{"fleet_node_credentials", "fleet_node_operations", "fleet_nodes", "fleet_credential_profiles"} {
			if _, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE namespace=$1 AND owner_id=$2", ns, f.UserID); err != nil {
				t.Errorf("router fixture %s cleanup: %v", table, err)
			}
			var remaining int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE namespace=$1 AND owner_id=$2", ns, f.UserID).Scan(&remaining); err != nil || remaining != 0 {
				t.Errorf("router fixture %s remaining=%d err=%v", table, remaining, err)
			}
		}
	})
	f.FleetProfile(t, ns)
	cfg := model.Config{Namespace: ns, Image: "fake-approved-image", MaxNodes: 2, Specs: map[string]model.Spec{"small": {CPUs: 2, MemoryBytes: 4294967296, Pids: 256, MaxRuns: 1}}}
	secret := []byte("test-only-012345678901234567890123456789")
	repo := store.New(pool, ns, store.WithProvisioningConfig(cfg))
	fleetServer := httptest.NewServer(fleet.NewService(repo, cfg, nil).Handler(secret))
	defer fleetServer.Close()
	path := filepath.Join(t.TempDir(), "service-key")
	if err := os.WriteFile(path, secret, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULTICA_CLOUD_URL", "")
	t.Setenv("MULTICA_LOCAL_FLEET_URL", fleetServer.URL)
	t.Setenv("MULTICA_LOCAL_FLEET_SECRET_FILE", path)
	router, _ := NewRouterWithOptions(pool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	token, err := generateTestJWT(f.UserID, "fake@test.invalid", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Workspace-ID", f.WorkspaceID)
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	w := call("POST", "/api/cloud-runtime/nodes", `{"instance_type":"small","name":"node"}`, "router-once")
	if w.Code != 202 {
		t.Fatalf("full router create=%d %s", w.Code, w.Body.String())
	}
	var node struct {
		ID          string `json:"id"`
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &node); err != nil || node.ID == "" || node.OperationID == "" {
		t.Fatalf("router node response: %s err=%v", w.Body.String(), err)
	}
}

func TestLocalFleetSaaSRouterCompatibility(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fleet-Service-Key") != "" || r.Header.Get("X-User-ID") != testUserID {
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/api/v1/billing/balance":
			w.Write([]byte(`{"balance":123}`))
		case "/api/v1/nodes":
			raw, _ := io.ReadAll(r.Body)
			if string(raw) != `{"instance_type":"legacy","image_id":"legacy-image"}` {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(201)
			w.Write([]byte("{}"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("MULTICA_CLOUD_URL", srv.URL)
	t.Setenv("MULTICA_LOCAL_FLEET_URL", "")
	t.Setenv("MULTICA_LOCAL_FLEET_SECRET_FILE", "missing")
	router, h := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	if !h.CloudBilling.Enabled() || !h.CloudRuntime.Enabled() || h.Entitlements == nil || !seatcapacity.CanRunWorker(h.SeatCapacity) {
		t.Fatal("SaaS policies disabled")
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{{"GET", "/api/cloud-billing/balance", "", 200}, {"POST", "/api/cloud-runtime/nodes", `{"instance_type":"legacy","image_id":"legacy-image"}`, 201}} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("SaaS path=%s code=%d body=%s", tc.path, w.Code, w.Body.String())
		}
	}
}

// A configured Redis positive entry must not override local Fleet revocation.
func TestLocalFleetPATIgnoresAvailableRedis(t *testing.T) {
	var revoked atomic.Bool
	secret := []byte("test-only-012345678901234567890123456789")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fleet-Service-Key") != string(secret) || r.Header.Get("X-User-ID") != "" {
			w.WriteHeader(401)
			return
		}
		if revoked.Load() {
			w.Write([]byte(`{"valid":false}`))
			return
		}
		w.Write([]byte(`{"valid":true,"owner_id":"11111111-1111-4111-8111-111111111111"}`))
	}))
	defer srv.Close()
	rdb, mock := redismock.NewClientMock()
	// Deliberately offers a stale valid identity; consuming it would accept revocation.
	for range 2 {
		mock.ExpectGet("mul:auth:mcn:" + auth.HashToken("mcn_fake")).SetVal(`{"o":"11111111-1111-4111-8111-111111111111"}`)
	}
	verifier := fleetPATVerifier("", LocalFleetConfig{URL: srv.URL, Secret: secret, Enabled: true}, rdb)
	if _, err := verifier.Verify(context.Background(), "mcn_fake", nil); err != nil {
		t.Fatal(err)
	}
	revoked.Store(true)
	if _, err := verifier.Verify(context.Background(), "mcn_fake", nil); !errors.Is(err, auth.ErrCloudPATInvalid) {
		t.Fatalf("local Redis accepted revoked PAT: %v", err)
	}
}
