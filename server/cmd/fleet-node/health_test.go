package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/fleet/model"
)

func observationServer(t *testing.T, stats string, status string) (*http.Client, string) {
	t.Helper()
	part := ""
	if stats != "" {
		part = ",\"report_queue_stats\":" + stats
	}
	raw := fmt.Sprintf(`{"daemon_id":"%s","status":"%s","active_task_count":2,"pending_terminal_report_count":99,"agents":["claude"],"workspaces":[{"id":"unit","runtimes":["one","two"]}]%s}`, testDaemonID, status, part)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, raw) }))
	t.Cleanup(server.Close)
	return server.Client(), server.URL
}
func TestReadObservationMapsNativeHealthAndOnlyNewStats(t *testing.T) {
	client, url := observationServer(t, `{"known":true,"pending":3,"failed":4}`, "running")
	got, err := ReadObservation(client, url)
	if err != nil || !got.Ready || got.DaemonID != testDaemonID || got.RuntimeCount != 2 || got.ActiveRuns != 2 || !got.ReportStatsKnown || got.PendingReports != 3 || got.FailedReports != 4 || got.StartEpoch != "" {
		t.Fatalf("native health mapping: %+v %v", got, err)
	}
}
func TestReadObservationMalformedOrAbsentStatsIsUnknown(t *testing.T) {
	for _, raw := range []string{"", "null", "[]", `{"known":true,"pending":-1,"failed":0}`, `{"known":true,"pending":0.5,"failed":0}`, `{"known":true,"pending":0}`, `{"known":"true","pending":0,"failed":0}`, `{"known":true,"pending":0,"failed":0,"extra":1}`, `{"known":true,"pending":0,"pending":1,"failed":0}`, `{"known":false,"pending":99,"failed":4}`} {
		t.Run(raw, func(t *testing.T) {
			client, url := observationServer(t, raw, "running")
			got, err := ReadObservation(client, url)
			if err != nil || !got.Ready || got.ReportStatsKnown || got.PendingReports != 0 || got.FailedReports != 0 {
				t.Fatalf("unknown stats altered liveness or inferred legacy zeros: %+v %v", got, err)
			}
		})
	}
}
func TestReadObservationMissingOrNullActiveCountIsUnknown(t *testing.T) {
	for _, count := range []string{"", ",\"active_task_count\":null"} {
		t.Run(count, func(t *testing.T) {
			raw := fmt.Sprintf(`{"daemon_id":"%s","status":"running","agents":["claude"],"workspaces":[{"id":"unit","runtimes":["one"]}]%s,"report_queue_stats":{"known":true,"pending":0,"failed":0}}`, testDaemonID, count)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, raw) }))
			defer server.Close()
			got, err := ReadObservation(server.Client(), server.URL)
			if err == nil || got.Ready || got.ReportStatsKnown {
				t.Fatalf("missing count fabricated idle evidence: %+v %v", got, err)
			}
		})
	}
}

func TestReadObservationDuplicateStatsObjectIsUnknown(t *testing.T) {
	raw := fmt.Sprintf(`{"daemon_id":"%s","status":"running","active_task_count":0,"agents":["claude"],"workspaces":[{"runtimes":["one"]}],"report_queue_stats":{"known":false,"pending":0,"failed":0},"report_queue_stats":{"known":true,"pending":0,"failed":0}}`, testDaemonID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, raw) }))
	defer server.Close()
	got, err := ReadObservation(server.Client(), server.URL)
	if err != nil || !got.Ready || got.ReportStatsKnown {
		t.Fatalf("duplicate stats fabricated maintenance proof: %+v %v", got, err)
	}
}

func TestFixedHealthWireHasOnlyAcceptedSixFields(t *testing.T) {
	raw, err := json.Marshal(observationWire(model.Observation{DaemonID: testDaemonID, Ready: true, RuntimeCount: 1, Agents: []string{"claude"}, ReportStatsKnown: true}))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		t.Fatal("wire decode")
	}
	if len(fields) != 6 {
		t.Fatalf("public CLI wire gained private fields: %s", raw)
	}
	for _, key := range []string{"daemon_id", "ready", "runtime_count", "active_runs", "agents", "report_queue_stats"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
}
func TestFixedCommandsRejectCallerArgumentsBeforeAnyExecution(t *testing.T) {
	for _, args := range [][]string{{}, {"other"}, {"run", "--daemon-id", "caller"}, {"health", "--url", "http://api.test"}, {"report-stats", "--workspaces-root", "/other"}, {"bootstrap", "--profile", "owner"}} {
		var out bytes.Buffer
		if err := command(args, &out); err == nil || out.Len() != 0 {
			t.Fatalf("caller command accepted: %v", args)
		}
	}
}

func TestReadObservationStartingIsNotReady(t *testing.T) {
	client, url := observationServer(t, `{"known":true,"pending":0,"failed":0}`, "starting")
	got, err := ReadObservation(client, url)
	if err != nil || got.Ready {
		t.Fatalf("starting = %+v %v", got, err)
	}
}
