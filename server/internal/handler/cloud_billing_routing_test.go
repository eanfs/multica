package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cloudruntime"
)

// These tests deliberately build a bare Handler instead of using testHandler:
// the billing client split is pure handler wiring, so it needs neither the
// database fixture nor the process-wide TestMain.
const billingRoutingUserID = "billing-routing-user"

// TestCloudBillingRejectsWhenNotConfigured pins that owner-credit billing never
// falls back to the cloud-runtime (Fleet) client. Local Fleet mode enables
// CloudRuntime while leaving CloudBilling disabled; the billing endpoint must
// answer 403 cloud_billing_not_configured instead of reaching Fleet.
func TestCloudBillingRejectsWhenNotConfigured(t *testing.T) {
	cases := []struct {
		name    string
		billing cloudRuntimeProxy
	}{
		{name: "nil client", billing: nil},
		{name: "disabled client", billing: &fakeCloudRuntimeProxy{enabled: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtimeProxy := &fakeCloudRuntimeProxy{
				enabled: true,
				resp:    &cloudruntime.Response{StatusCode: http.StatusOK, Body: []byte(`{"source":"runtime"}`)},
			}
			h := &Handler{CloudRuntime: runtimeProxy, CloudBilling: tc.billing}

			req := httptest.NewRequest(http.MethodGet, "/api/cloud-billing/balance", nil)
			req.Header.Set("X-User-ID", billingRoutingUserID)
			w := httptest.NewRecorder()

			h.GetCloudBillingBalance(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v (body=%s)", err, w.Body.String())
			}
			if body["code"] != "cloud_billing_not_configured" {
				t.Fatalf("code = %q, want cloud_billing_not_configured (body=%s)", body["code"], w.Body.String())
			}
			if runtimeProxy.called {
				t.Fatal("billing request must not reach the cloud-runtime client")
			}
		})
	}
}

// TestCloudBillingUsesBillingClient proves the positive direction with real
// clients pointed at two distinct servers: the billing endpoint dials only the
// CloudBilling URL and returns its response untouched.
func TestCloudBillingUsesBillingClient(t *testing.T) {
	var runtimeCalls, billingCalls int
	var billingPath string

	runtimeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtimeCalls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"source":"runtime"}`))
	}))
	defer runtimeSrv.Close()
	billingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		billingCalls++
		billingPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"source":"billing"}`))
	}))
	defer billingSrv.Close()

	h := &Handler{
		CloudRuntime: cloudruntime.NewClient(cloudruntime.Config{BaseURL: runtimeSrv.URL, Timeout: time.Second}),
		CloudBilling: cloudruntime.NewClient(cloudruntime.Config{BaseURL: billingSrv.URL, Timeout: time.Second}),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/cloud-billing/balance", nil)
	req.Header.Set("X-User-ID", billingRoutingUserID)
	w := httptest.NewRecorder()

	h.GetCloudBillingBalance(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != `{"source":"billing"}` {
		t.Fatalf("body = %q, want the CloudBilling response", got)
	}
	if billingCalls != 1 {
		t.Fatalf("billing server calls = %d, want 1", billingCalls)
	}
	if runtimeCalls != 0 {
		t.Fatalf("runtime server calls = %d, want 0 (billing must not use CloudRuntime)", runtimeCalls)
	}
	if billingPath != "/api/v1/billing/balance" {
		t.Fatalf("billing upstream path = %q, want /api/v1/billing/balance", billingPath)
	}
}
