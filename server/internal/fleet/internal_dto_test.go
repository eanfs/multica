package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/fleet/model"
)

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
