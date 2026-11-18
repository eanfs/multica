package fleet

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const httpTestSecret = "top-secret"

func serviceRequest(method, path, body, authorization, userID string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}
	return req
}

func TestHTTPRejectsMissingOrWrongServiceSecret(t *testing.T) {
	handler := NewHTTP(&Service{secret: httpTestSecret})
	cases := []struct {
		name          string
		authorization string
	}{
		{"missing", ""},
		{"wrong", "Bearer wrong"},
		{"empty bearer", "Bearer "},
		{"longer secret", "Bearer " + httpTestSecret + "-extra"},
		{"raw secret without scheme", httpTestSecret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := serviceRequest(http.MethodGet, "/api/v1/nodes", "", tc.authorization, "owner-1")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
		})
	}
}

func TestHTTPRequiresUserHeaderAfterServiceAuth(t *testing.T) {
	handler := NewHTTP(&Service{secret: httpTestSecret})
	req := serviceRequest(http.MethodGet, "/api/v1/nodes", "", "Bearer "+httpTestSecret, "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHTTPHealthzNeedsNoServiceSecret(t *testing.T) {
	handler := NewHTTP(nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", w.Code)
	}
}

func TestHTTPReadyzWithoutDockerIsUnavailable(t *testing.T) {
	handler := NewHTTP(nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", w.Code)
	}
}

func TestHTTPReadyzRequiresStoreAndDocker(t *testing.T) {
	handler := NewHTTP(nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz without service = %d, want 503", w.Code)
	}

	handler = NewHTTP(&Service{provider: newFakeDocker()})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz without store = %d, want 503", w.Code)
	}

	handler = NewHTTP(&Service{store: &store.Store{}, provider: newFakeDocker()})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with unreachable store = %d, want 503", w.Code)
	}
}

// The node contract promotes the diagnostics that used to live only in
// metadata, so a client reads one shape from every provider.
func TestNodeResponsePromotesDiagnosticsAndOperation(t *testing.T) {
	now := time.Now()
	node := db.FleetNode{
		ID:              pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		OwnerID:         pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
		DaemonID:        "daemon-1",
		ContainerID:     "container-1",
		TargetState:     StateRunning,
		ObservedState:   StateRunning,
		DaemonReady:     true,
		HealthCheckedAt: pgtype.Timestamptz{Time: now, Valid: true},
		StatusDetail:    []byte(`{"cli_installed":true}`),
		Spec:            []byte(`{"id":"standard"}`),
	}
	op := &db.FleetNodeOperation{Operation: "stop", Phase: OpPhasePrepared}

	response := NewService(Config{}, nil, nil, nil, "").nodeResponse(node, op)
	if response.DaemonID != "daemon-1" {
		t.Fatalf("daemon id = %q", response.DaemonID)
	}
	if string(response.StatusDetail) != `{"cli_installed":true}` {
		t.Fatalf("status detail = %s", response.StatusDetail)
	}
	if response.Operation == nil || response.Operation.Name != "stop" || response.Operation.Phase != OpPhasePrepared {
		t.Fatalf("operation = %+v", response.Operation)
	}

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal node: %v", err)
	}
	if !strings.Contains(string(encoded), `"daemon_id":"daemon-1"`) ||
		!strings.Contains(string(encoded), `"status_detail":{"cli_installed":true}`) ||
		!strings.Contains(string(encoded), `"operation":{"name":"stop","phase":"prepared"}`) {
		t.Fatalf("encoded node lacks diagnostics: %s", encoded)
	}
}

func TestHTTPReadyzPingsDependencies(t *testing.T) {
	pool := serviceTestPool(t)
	provider := newFakeDocker()
	handler := NewHTTP(&Service{store: store.New(pool), provider: provider})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200", w.Code)
	}

	provider.pingErr = errors.New("docker is down")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with docker error = %d, want 503", w.Code)
	}
}

func TestHTTPUnconfiguredWriteEndpointsReturn503(t *testing.T) {
	handler := NewHTTP(&Service{
		secret: httpTestSecret,
		cfg: Config{
			MaxNodesPerOwner: 2,
			Specs:            []Spec{{ID: "standard", CPU: 2, MemoryMB: 4096, PidsLimit: 256}},
		},
		creds: serviceTestCredentials("owner-1"),
	})
	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/nodes", "{\"name\":\"n\",\"spec_id\":\"standard\"}"},
		{http.MethodDelete, "/api/v1/nodes", "{\"instance_id\":\"c1\"}"},
		{http.MethodPost, "/api/v1/nodes/start", "{\"instance_id\":\"c1\"}"},
		{http.MethodPost, "/api/v1/nodes/stop", "{\"instance_id\":\"c1\"}"},
		{http.MethodPost, "/api/v1/nodes/reboot", "{\"instance_id\":\"c1\"}"},
		{http.MethodPost, "/api/v1/nodes/status", "{\"instance_id\":\"c1\"}"},
		{http.MethodPost, "/api/v1/nodes/diagnose", "{\"instance_id\":\"c1\"}"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := serviceRequest(tc.method, tc.path, tc.body, "Bearer "+httpTestSecret, "owner-1")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s %s = %d, want 503", tc.method, tc.path, w.Code)
			}
		})
	}
}

func TestHTTPPatVerifyDoesNotRequireUserHeader(t *testing.T) {
	handler := NewHTTP(&Service{secret: httpTestSecret})
	req := serviceRequest(http.MethodPost, "/api/v1/pat/verify", "{\"token\":\"mcn_x\"}", "Bearer "+httpTestSecret, "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("pat verify without service = %d, want 503", w.Code)
	}
}

func TestHTTPCreateNodeRejectsUnknownSpec(t *testing.T) {
	svc := NewService(Config{Specs: []Spec{{ID: "standard", CPU: 2, MemoryMB: 4096, PidsLimit: 256}}}, nil, nil, nil, httpTestSecret)
	handler := NewHTTP(svc)
	req := serviceRequest(http.MethodPost, "/api/v1/nodes", "{\"name\":\"n\",\"spec_id\":\"nope\"}", "Bearer "+httpTestSecret, "owner-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if !strings.Contains(w.Body.String(), "spec") {
		t.Fatalf("body = %q, want spec error", w.Body.String())
	}
}

func TestHTTPCreateNodeRejectsUnknownFields(t *testing.T) {
	svc := NewService(Config{Specs: []Spec{{ID: "standard", CPU: 2, MemoryMB: 4096, PidsLimit: 256}}}, nil, nil, nil, httpTestSecret)
	handler := NewHTTP(svc)
	body := "{\"name\":\"n\",\"spec_id\":\"standard\",\"image_id\":\"evil\"}"
	req := serviceRequest(http.MethodPost, "/api/v1/nodes", body, "Bearer "+httpTestSecret, "owner-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHTTPRejectsTrailingJSON(t *testing.T) {
	svc := NewService(Config{Specs: []Spec{{ID: "standard", CPU: 2, MemoryMB: 4096, PidsLimit: 256}}}, nil, nil, nil, httpTestSecret)
	handler := NewHTTP(svc)
	body := "{\"name\":\"n\",\"spec_id\":\"nope\"}{\"name\":\"second\"}"
	req := serviceRequest(http.MethodPost, "/api/v1/nodes", body, "Bearer "+httpTestSecret, "owner-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON = %d, want 400", w.Code)
	}

	// Trailing whitespace is still a single JSON value.
	req = serviceRequest(http.MethodPost, "/api/v1/nodes",
		"{\"name\":\"n\",\"spec_id\":\"nope\"}\n", "Bearer "+httpTestSecret, "owner-1")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("trailing whitespace = %d, want 422 (decoded, unknown spec)", w.Code)
	}
}

func TestHTTPDeleteAndDiagnoseAcceptNodeID(t *testing.T) {
	// A nil store returns 503 only after the body decoded and the node
	// reference was accepted; a rejected body would be 400.
	svc := &Service{secret: httpTestSecret}
	handler := NewHTTP(svc)
	for _, path := range []string{"/api/v1/nodes", "/api/v1/nodes/diagnose"} {
		method := http.MethodDelete
		if path == "/api/v1/nodes/diagnose" {
			method = http.MethodPost
		}
		req := serviceRequest(method, path,
			"{\"node_id\":\"11111111-1111-1111-1111-111111111111\"}",
			"Bearer "+httpTestSecret, "owner-1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s node_id body = %d, want 503", path, w.Code)
		}
	}
}

func TestHTTPServiceInfoHidesPrivateConfig(t *testing.T) {
	svc := NewService(Config{
		Namespace:        "wt-svc",
		SecretFile:       "/run/secrets/fleet-secret",
		DatabaseURL:      "postgres://fleet:password@db.internal/multica",
		CredentialsFile:  "/run/secrets/model-credentials.json",
		MaxNodesPerOwner: 2,
		Specs:            []Spec{{ID: "standard", CPU: 2, MemoryMB: 4096, PidsLimit: 256, MaxConcurrent: 1}},
	}, nil, nil, nil, httpTestSecret)
	handler := NewHTTP(svc)
	req := serviceRequest(http.MethodGet, "/api/v1/", "", "Bearer "+httpTestSecret, "owner-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, secret := range []string{"fleet-secret", "postgres://", "model-credentials", httpTestSecret} {
		if strings.Contains(body, secret) {
			t.Fatalf("service info leaks %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "\"provider\":\"docker\"") {
		t.Fatalf("service info missing provider: %s", body)
	}
}

func TestWriteServiceErrorMapsSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", ErrNotFound, http.StatusNotFound},
		{"invalid request", ErrInvalidRequest, http.StatusBadRequest},
		{"spec", ErrSpecNotAllowed, http.StatusUnprocessableEntity},
		{"profile missing", ErrProfileMissing, http.StatusUnprocessableEntity},
		{"profile invalid", ErrProfileInvalid, http.StatusUnprocessableEntity},
		{"node busy", ErrNodeBusy, http.StatusConflict},
		{"conflict", ErrConflict, http.StatusConflict},
		{"limit", store.ErrLimitReached, http.StatusConflict},
		{"docker", ErrDockerUnavailable, http.StatusServiceUnavailable},
		{"image", ErrImageMissing, http.StatusServiceUnavailable},
		{"uncertain", ErrStateUncertain, http.StatusServiceUnavailable},
		{"not configured", ErrNotConfigured, http.StatusServiceUnavailable},
		{"unknown", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeServiceError(w, tc.err)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}
