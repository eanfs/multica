package fleetclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestClientStampsServiceSecret pins the service-to-service auth contract:
// every request the client sends to the fleet origin carries the configured
// secret as a Bearer token, without callers having to set it per request.
func TestClientStampsServiceSecret(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, Secret: "s3cret", Timeout: time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/v1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "Bearer s3cret" {
		t.Fatalf("authorization = %q", got)
	}
}

// TestClientDoesNotLeakSecretOnRedirectToOtherHost pins that a redirect off
// the fleet origin can never receive the service secret, even though the
// transport is the only place the header is attached.
func TestClientDoesNotLeakSecretOnRedirectToOtherHost(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("secret leaked to redirect target")
		}
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, Secret: "s3cret", Timeout: time.Second})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := client.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// TestClientKeepsSecretOnSameOriginRedirect guards the inverse: redirects
// that stay on the fleet origin still authenticate, so a Fleet route alias
// keeps working.
func TestClientKeepsSecretOnSameOriginRedirect(t *testing.T) {
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, Secret: "s3cret", Timeout: time.Second})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/moved", nil)
	resp, err := client.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "Bearer s3cret" {
		t.Fatalf("authorization after same-origin redirect = %q", got)
	}
}
