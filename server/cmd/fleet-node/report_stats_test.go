package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

func TestOfflineReportStatsMatchesOnlineLayout(t *testing.T) {
	data, _, _ := nodeFixture(t)
	dir := filepath.Join(data, "workspaces", ".pending-terminal-reports", "v1", "foreign")
	if err := os.MkdirAll(filepath.Join(dir, "failed"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pending"), []byte("not read"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "failed", "report"), []byte("not read"), 0600); err != nil {
		t.Fatal(err)
	}
	wire, err := offlineReportStatsAt(data)
	if err != nil || !wire.Reports.Known || wire.Reports.Pending != 1 || wire.Reports.Failed != 1 {
		t.Fatalf("offline stats: %+v %v", wire, err)
	}
	online, err := daemon.ScanReportQueueStats(filepath.Join(data, "workspaces"))
	if err != nil || online != wire.Reports {
		t.Fatalf("different online layout: %+v %v", online, err)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	checkFixture(t, json.Unmarshal(raw, &fields))
	if len(fields) != 2 || len(fields["manifest"]) == 0 || len(fields["report_queue_stats"]) == 0 {
		t.Fatalf("offline wire keys: %s", raw)
	}
	if model.ValidateLayoutManifest(wire.Manifest, model.LayoutIdentity{Namespace: "unit", FleetID: "unit-fleet", NodeID: "00000000-0000-4000-8000-000000000002", DaemonID: testDaemonID}) != nil {
		t.Fatal("manifest identity lost")
	}
}
func TestOfflineMissingManifestIsUnknown(t *testing.T) {
	data, _, _ := nodeFixture(t)
	if err := os.Remove(filepath.Join(data, "fleet-layout.json")); err != nil {
		t.Fatal(err)
	}
	got, err := offlineReportStatsAt(data)
	if err == nil || got.Reports.Known {
		t.Fatalf("missing manifest fabricated known: %+v %v", got, err)
	}
}
func TestOfflineRejectsWrongMountAndLinkedRoots(t *testing.T) {
	for _, kind := range []string{"missing-root", "symlink-root", "missing-workspaces", "symlink-workspaces", "wrong-mount", "manifest-mode", "manifest-symlink"} {
		t.Run(kind, func(t *testing.T) {
			data, _, _ := nodeFixture(t)
			switch kind {
			case "missing-root":
				data = filepath.Join(data, "missing")
			case "symlink-root":
				link := filepath.Join(t.TempDir(), "data")
				checkFixture(t, os.Symlink(data, link))
				data = link
			case "missing-workspaces":
				checkFixture(t, os.Remove(filepath.Join(data, "workspaces")))
			case "symlink-workspaces":
				checkFixture(t, os.Remove(filepath.Join(data, "workspaces")))
				checkFixture(t, os.Symlink(t.TempDir(), filepath.Join(data, "workspaces")))
			case "wrong-mount":
				raw := readFixture(t, filepath.Join(data, "fleet-layout.json"))
				var m model.LayoutManifestData
				checkFixture(t, json.Unmarshal(raw, &m))
				m.DataMount = "/wrong"
				raw = encodeFixture(t, m)
				checkFixture(t, os.WriteFile(filepath.Join(data, "fleet-layout.json"), raw, 0600))
			case "manifest-mode":
				checkFixture(t, os.Chmod(filepath.Join(data, "fleet-layout.json"), 0644))
			case "manifest-symlink":
				p := filepath.Join(data, "fleet-layout.json")
				target := filepath.Join(data, "target")
				checkFixture(t, os.Rename(p, target))
				checkFixture(t, os.Symlink(target, p))
			}
			got, err := offlineReportStatsAt(data)
			if err == nil || got.Reports.Known {
				t.Fatalf("unsafe layout trusted: %+v %v", got, err)
			}
		})
	}
}
func TestOfflineEmptyScanDoesNotCreateOrStartAnything(t *testing.T) {
	data, _, _ := nodeFixture(t)
	got, err := offlineReportStatsAt(data)
	if err != nil || got.Reports != (daemon.ReportQueueStats{Known: true}) {
		t.Fatalf("offline empty = %+v %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Join(data, "workspaces"))
	if err != nil || len(entries) != 0 {
		t.Fatal("offline scanner mutated node")
	}
}
