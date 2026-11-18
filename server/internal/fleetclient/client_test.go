package fleetclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
// the fleet origin is never followed: the service secret cannot leak, and the
// redirect response is returned as-is.
func TestClientDoesNotLeakSecretOnRedirectToOtherHost(t *testing.T) {
	var otherCalls int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherCalls++
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
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (redirect must not be followed)", resp.StatusCode, http.StatusTemporaryRedirect)
	}
	if otherCalls != 0 {
		t.Fatalf("cross-origin redirect target called %d times, want 0", otherCalls)
	}
}

// TestClientDoesNotReplayBodyAcrossOrigins pins the 307/308 body replay rule:
// the redirect response is returned before the body is re-sent anywhere else.
func TestClientDoesNotReplayBodyAcrossOrigins(t *testing.T) {
	var otherCalls int
	var otherBody string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherCalls++
		body, _ := io.ReadAll(r.Body)
		otherBody = string(body)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, Secret: "s3cret", Timeout: time.Second})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/v1/nodes", strings.NewReader(`{"instance_type":"g5.xlarge"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if otherCalls != 0 {
		t.Fatalf("cross-origin redirect target called %d times, want 0", otherCalls)
	}
	if otherBody != "" {
		t.Fatalf("request body replayed to redirect target: %q", otherBody)
	}
}

// TestVerifyHTTPClientSharesTransportAndCapsTimeout pins the verifier wiring:
// reuse the fleet connection pool but bound auth at VerifyTimeout.
func TestVerifyHTTPClientSharesTransportAndCapsTimeout(t *testing.T) {
	client := New(Config{BaseURL: "http://fleet", Secret: "s3cret", Timeout: 35 * time.Second})
	verify := client.VerifyHTTPClient()
	if verify.Timeout != VerifyTimeout {
		t.Fatalf("verify timeout = %s, want %s", verify.Timeout, VerifyTimeout)
	}
	if verify.Transport != client.HTTPClient().Transport {
		t.Fatal("verify client must reuse the fleet transport")
	}
	if verify.CheckRedirect == nil {
		t.Fatal("verify client must keep the fleet redirect policy")
	}
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
