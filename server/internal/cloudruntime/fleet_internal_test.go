package cloudruntime

import (
	"context"
	"encoding/json"
	"github.com/multica-ai/multica/server/internal/fleet"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPrivateReviewClientStrictWire(t *testing.T) {
	owner := "11111111-1111-4111-8111-111111111111"
	node := "22222222-2222-4222-8222-222222222222"
	op := "33333333-3333-4333-8333-333333333333"
	ref := model.OperationRef{Namespace: "owned-ns", NodeID: util.MustParseUUID(node), OperationID: util.MustParseUUID(op), Generation: 7, Action: model.Delete}
	good := fleet.OperationReviewResponseDTO{Namespace: ref.Namespace, OwnerID: owner, NodeID: node, OperationID: op, Generation: 7, Action: ref.Action, Phase: "queued", Approved: true}
	raw, e := json.Marshal(good)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name      string
		status    int
		transform func(string) string
		valid     bool
	}{
		{"valid", 200, func(s string) string { return s }, true},
		{"non-success-valid-body", 409, func(s string) string { return s }, false},
		{"malformed", 200, func(string) string { return "{" }, false},
		{"wrong-owner", 200, func(s string) string { return strings.Replace(s, owner, node, 1) }, false},
		{"wrong-namespace", 200, func(s string) string { return strings.Replace(s, "owned-ns", "foreign", 1) }, false},
		{"wrong-node", 200, func(s string) string { return strings.Replace(s, node, owner, 1) }, false},
		{"wrong-operation", 200, func(s string) string { return strings.Replace(s, op, owner, 1) }, false},
		{"stale-generation", 200, func(s string) string { return strings.Replace(s, ":7", ":6", 1) }, false},
		{"wrong-action", 200, func(s string) string { return strings.Replace(s, "delete", "stop", 1) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/internal/local-fleet/operations/review" || r.Header.Get("X-User-ID") != owner || r.Header.Get("X-Fleet-Service-Key") != "private-key-012345678901234567890123456789" {
					t.Error("wrong authenticated fixed route")
				}
				var request map[string]any
				if e := json.NewDecoder(r.Body).Decode(&request); e != nil || len(request) != 5 {
					t.Error("request contract not five fields", e)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.transform(string(raw))))
			}))
			defer server.Close()
			c := NewClient(Config{BaseURL: server.URL, ServiceSecret: []byte("private-key-012345678901234567890123456789")})
			got, e := c.ReviewOperation(context.Background(), owner, ref)
			if tc.valid {
				if e != nil || got.ID != ref.OperationID || got.NodeID != ref.NodeID || got.OwnerID != util.MustParseUUID(owner) || got.Action != ref.Action || got.Generation != ref.Generation || got.Phase != good.Phase || got.Approved != good.Approved {
					t.Fatal("valid private wire failed", e)
				}
			} else if e == nil {
				t.Fatal("invalid private wire accepted", got)
			}
		})
	}
}
func TestPrivateDiagnosticClientEchoAndMalformed(t *testing.T) {
	owner := "11111111-1111-4111-8111-111111111111"
	ref := model.OperationRef{Namespace: "owned-ns", NodeID: util.MustParseUUID("22222222-2222-4222-8222-222222222222"), OperationID: util.MustParseUUID("33333333-3333-4333-8333-333333333333"), Generation: 7, Action: model.Stop}
	good := fleet.DiagnosticResponseDTO{Request: operationRequest(ref), Observation: model.Observation{ReportStatsKnown: true, ObservedAt: time.Now().UTC(), ContainerID: "container", DaemonID: "daemon", StartEpoch: "epoch", Ready: true, Status: "running"}}
	raw, e := json.Marshal(good)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name      string
		status    int
		transform func(string) string
		valid     bool
	}{
		{"valid", 200, func(s string) string { return s }, true},
		{"non-success", 409, func(s string) string { return s }, false},
		{"wrong-echo", 200, func(s string) string { return strings.Replace(s, "owned-ns", "foreign", 1) }, false},
		{"extra-observation", 200, func(s string) string { return strings.Replace(s, "\"container_id\"", "\"unknown\"", 1) }, false},
		{"duplicate-observation", 200, func(s string) string { return strings.Replace(s, "\"ready\":true", "\"ready\":true,\"ready\":true", 1) }, false},
		{"negative-count", 200, func(s string) string { return strings.Replace(s, "\"active_runs\":0", "\"active_runs\":-1", 1) }, false},
		{"missing-report-stat", 200, func(s string) string { return strings.Replace(s, ",\"report_stats_known\":true", "", 1) }, false},
		{"null-report-stat", 200, func(s string) string {
			return strings.Replace(s, "\"report_stats_known\":true", "\"report_stats_known\":null", 1)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/internal/v1/nodes/diagnose" || r.Header.Get("X-User-ID") != owner {
					t.Error("diagnostic route identity")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.transform(string(raw))))
			}))
			defer server.Close()
			c := NewClient(Config{BaseURL: server.URL, ServiceSecret: []byte("private-key-012345678901234567890123456789")})
			_, e := c.DiagnoseNode(context.Background(), owner, ref)
			if tc.valid && e != nil {
				t.Fatal(e)
			}
			if !tc.valid && e == nil {
				t.Fatal("malformed diagnostic accepted")
			}
		})
	}
}
