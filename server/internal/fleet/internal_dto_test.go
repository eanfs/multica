package fleet

import (
	"encoding/json"
	"github.com/multica-ai/multica/server/internal/util"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/fleet/model"
)

func TestOperationReviewResponseDTO(t *testing.T) {
	owner := util.MustParseUUID("11111111-1111-4111-8111-111111111111")
	node := util.MustParseUUID("22222222-2222-4222-8222-222222222222")
	opID := util.MustParseUUID("33333333-3333-4333-8333-333333333333")
	n := model.Node{Namespace: "owned", OwnerID: owner, ID: node, Generation: 7}
	op := model.Operation{ID: opID, NodeID: node, OwnerID: owner, Action: model.Delete, Generation: 7, Phase: "queued", Approved: true, IdempotencyKey: "never-wire", RequestHash: "never-wire", PriorDesired: "never-wire"}
	dto, e := NewReviewResponse(n, op)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(dto)
	if e != nil {
		t.Fatal(e)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if len(fields) != 8 || strings.Contains(string(raw), "never-wire") {
		t.Fatalf("projection=%s", raw)
	}
	var round OperationReviewResponseDTO
	if e = json.Unmarshal(raw, &round); e != nil || round != dto {
		t.Fatal("roundtrip", e)
	}
	for key := range fields {
		for _, value := range []string{"missing", "null"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				var changed map[string]json.RawMessage
				_ = json.Unmarshal(raw, &changed)
				if value == "missing" {
					delete(changed, key)
				} else {
					changed[key] = json.RawMessage("null")
				}
				bad, _ := json.Marshal(changed)
				var out OperationReviewResponseDTO
				if e := json.Unmarshal(bad, &out); e == nil {
					t.Fatalf("invalid field accepted=%s", bad)
				}
			})
		}
	}
	for _, tc := range []struct{ name, key, value string }{
		{"zero-owner", "owner_id", "\" 00000000-0000-0000-0000-000000000000 \""},
		{"zero-node", "node_id", "\"00000000-0000-0000-0000-000000000000\""},
		{"zero-operation", "operation_id", "\"00000000-0000-0000-0000-000000000000\""},
		{"nonuuid", "owner_id", "\"not-uuid\""},
		{"zero-generation", "generation", "0"}, {"negative-generation", "generation", "-1"}, {"fractional-generation", "generation", "1.5"}, {"string-generation", "generation", "\"7\""},
		{"unknown-action", "action", "\"force\""}, {"blank-namespace", "namespace", "\"\""},
		{"unknown-phase", "phase", "\"applying\""}, {"node-status-not-phase", "phase", "\"running\""}, {"string-approved", "approved", "\"true\""}, {"unapproved-queued", "approved", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed map[string]json.RawMessage
			_ = json.Unmarshal(raw, &changed)
			changed[tc.key] = json.RawMessage(tc.value)
			bad, _ := json.Marshal(changed)
			var out OperationReviewResponseDTO
			if e := json.Unmarshal(bad, &out); e == nil {
				t.Fatal("malformed response accepted", string(bad))
			}
		})
	}
	for _, bad := range []string{"{", string(raw) + " {}", strings.TrimSuffix(string(raw), "}") + ",\"node_token\":\"secret\"}", strings.TrimSuffix(string(raw), "}") + ",\"approved\":true}"} {
		var out OperationReviewResponseDTO
		if e := json.Unmarshal([]byte(bad), &out); e == nil {
			t.Fatal("invalid structure accepted", bad)
		}
	}
}

// Go field names leaking into the private wire break Task7/10 without failing public DTO tests.
func TestDiagnosticDTOObservationWire(t *testing.T) {
	dto := DiagnosticResponseDTO{Request: OperationRequestDTO{Namespace: "ns", NodeID: "node", OperationID: "op", Generation: 2, Action: model.Stop}, Observation: model.Observation{ContainerID: "container", Status: "running", DaemonID: "daemon", StartEpoch: "epoch", Ready: true, RuntimeCount: 1, ActiveRuns: 2, PendingReports: 3, FailedReports: 4, ReportStatsKnown: true, ObservedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Offline: false, DataVolume: "volume", LayoutVersion: "1"}}
	raw, e := json.Marshal(dto)
	if e != nil {
		t.Fatal(e)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(raw, &out)
	var obs map[string]any
	_ = json.Unmarshal(out["observation"], &obs)
	for _, key := range []string{"container_id", "status", "daemon_id", "start_epoch", "ready", "runtime_count", "active_runs", "pending_reports", "failed_reports", "report_stats_known", "observed_at", "offline", "data_volume", "layout_version"} {
		if _, ok := obs[key]; !ok {
			t.Fatalf("missing wire field %s: %s", key, raw)
		}
	}
	if len(obs) != 14 || strings.Contains(string(raw), "node_token") {
		t.Fatalf("wire=%s", raw)
	}
	var round DiagnosticResponseDTO
	if e = json.Unmarshal(raw, &round); e != nil || round.Request.Generation != 2 || round.Observation.PendingReports != 3 {
		t.Fatalf("roundtrip=%v %v", round, e)
	}
}
