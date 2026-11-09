package fleet

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/fleet/store"
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
	handler := NewHTTP(nil, httpTestSecret)
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
	handler := NewHTTP(nil, httpTestSecret)
	req := serviceRequest(http.MethodGet, "/api/v1/nodes", "", "Bearer "+httpTestSecret, "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHTTPHealthzNeedsNoServiceSecret(t *testing.T) {
	handler := NewHTTP(nil, httpTestSecret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", w.Code)
	}
}

func TestHTTPReadyzWithoutDockerIsUnavailable(t *testing.T) {
	handler := NewHTTP(nil, httpTestSecret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", w.Code)
	}
}

func TestHTTPReadyzRequiresStoreAndDocker(t *testing.T) {
	handler := NewHTTP(nil, httpTestSecret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz without service = %d, want 503", w.Code)
	}

	handler = NewHTTP(&Service{provider: newFakeDocker()}, httpTestSecret)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz without store = %d, want 503", w.Code)
	}

	handler = NewHTTP(&Service{store: &store.Store{}, provider: newFakeDocker()}, httpTestSecret)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with unreachable store = %d, want 503", w.Code)
	}
}

func TestHTTPReadyzPingsDependencies(t *testing.T) {
	pool := serviceTestPool(t)
	provider := newFakeDocker()
	handler := NewHTTP(&Service{store: store.New(pool), provider: provider}, httpTestSecret)

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

func TestHTTPNilServiceWriteEndpointsReturn503(t *testing.T) {
	handler := NewHTTP(nil, httpTestSecret)
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
	handler := NewHTTP(nil, httpTestSecret)
	req := serviceRequest(http.MethodPost, "/api/v1/pat/verify", "{\"token\":\"mcn_x\"}", "Bearer "+httpTestSecret, "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("pat verify without service = %d, want 503", w.Code)
	}
}

func TestHTTPCreateNodeRejectsUnknownSpec(t *testing.T) {
	svc := NewService(Config{Specs: []Spec{{ID: "standard", CPU: 2, MemoryMB: 4096, PidsLimit: 256}}}, nil, nil, nil, httpTestSecret)
	handler := NewHTTP(svc, httpTestSecret)
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
	handler := NewHTTP(svc, httpTestSecret)
	body := "{\"name\":\"n\",\"spec_id\":\"standard\",\"image_id\":\"evil\"}"
	req := serviceRequest(http.MethodPost, "/api/v1/nodes", body, "Bearer "+httpTestSecret, "owner-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
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
	handler := NewHTTP(svc, httpTestSecret)
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
