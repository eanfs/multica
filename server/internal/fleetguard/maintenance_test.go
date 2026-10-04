package fleetguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/multica-ai/multica/server/internal/fleet"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"testing"
	"time"
)

func TestReportsBlockMaintenance(t *testing.T) {
	for _, tc := range []struct {
		name string
		obs  model.Observation
		want bool
	}{
		{"unknown", model.Observation{}, true},
		{"idle", model.Observation{ReportStatsKnown: true}, false},
		{"active", model.Observation{ReportStatsKnown: true, ActiveRuns: 1}, true},
		{"pending", model.Observation{ReportStatsKnown: true, PendingReports: 1}, true},
		{"failed", model.Observation{ReportStatsKnown: true, FailedReports: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := BusyObservation(tc.obs); got != tc.want {
				t.Errorf("busy=%v want=%v", got, tc.want)
			}
		})
	}
}

// Unknown proof must keep the committed barrier, even when untrusted counts claim busy.
func TestFleetMaintenanceUnknownAndBusy(t *testing.T) {
	for _, kind := range []string{"diagnostic-error", "unknown-report", "stale", "old-epoch", "foreign-volume", "known-pending", "known-failed", "idle"} {
		t.Run(kind, func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := "task7-proof-" + f.UserID
			t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
			id := f.FleetNode(t, ns, testutil.Cols{"status": "running", "container_id": "container-current", "start_epoch": "epoch-current", "data_volume": "owned-data"})
			f.Cleanup(t, "DELETE FROM fleet_node_operations WHERE namespace=$1 AND owner_id=$2", ns, f.UserID)
			s := store.New(pool, ns)
			ctx := context.Background()
			owner := util.MustParseUUID(f.UserID)
			node := util.MustParseUUID(id)
			token, _, e := s.MintNodeToken(ctx, node)
			if e != nil {
				t.Fatal(e)
			}
			f.Cleanup(t, "DELETE FROM fleet_node_credentials WHERE namespace=$1 AND owner_id=$2", ns, f.UserID)
			m := Maintainer{Repo: s, Diagnose: func(c context.Context, n model.Node, ref model.OperationRef) (model.Observation, error) {
				// Exclusive lock NOWAIT-equivalent proves both committed prepare and no retained transaction.
				tx, e := pool.Begin(c)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback(context.Background())
				var free bool
				e = tx.QueryRow(c, "SELECT pg_try_advisory_xact_lock(hashtextextended($1||':'||$2,0))", ns, id).Scan(&free)
				if e != nil || !free {
					t.Fatal("diagnosis under database lock", e)
				}
				persisted, e := s.GetOperation(c, owner, ref.OperationID)
				if e != nil || persisted.Phase != "preparing" || persisted.Approved {
					t.Fatal("diagnose before prepare commit", e)
				}
				if _, e = s.VerifyNodeToken(c, token); e != nil {
					t.Fatal("accepted callback credential revoked", e)
				}
				o := model.Observation{ContainerID: n.ContainerID, DaemonID: n.DaemonID, StartEpoch: n.StartEpoch, Status: "running", Ready: true, ReportStatsKnown: true, ObservedAt: time.Now().UTC()}
				switch kind {
				case "diagnostic-error":
					return o, errors.New("fake unavailable")
				case "unknown-report":
					o.ReportStatsKnown = false
					o.PendingReports = 5
				case "stale":
					o.ObservedAt = time.Now().Add(-time.Minute)
					o.PendingReports = 5
				case "old-epoch":
					o.StartEpoch = "previous"
					o.PendingReports = 5
				case "foreign-volume":
					o.Offline = true
					o.Status = "missing"
					o.StartEpoch = ""
					o.DataVolume = "foreign"
					o.LayoutVersion = "1"
					o.PendingReports = 5
				case "known-pending":
					o.PendingReports = 1
				case "known-failed":
					o.FailedReports = 1
				}
				return o, nil
			}}
			op, e := m.Request(ctx, owner, node, model.Delete, "delete")
			want := model.ErrUnknownHealth
			if kind == "known-pending" || kind == "known-failed" {
				want = model.ErrBusy
			}
			if kind == "idle" {
				want = nil
			}
			if !errors.Is(e, want) {
				t.Fatalf("proof outcome=%v want=%v", e, want)
			}
			n, e := s.GetNode(ctx, owner, node)
			if e != nil {
				t.Fatal(e)
			}
			if want == model.ErrUnknownHealth {
				if !n.Maintenance || n.Revoked || n.Desired != "running" || op.Approved || op.Phase != "preparing" {
					t.Fatalf("unknown lost retained barrier n=%+v op=%+v", n, op)
				}
			}
			if want == model.ErrBusy {
				if n.Maintenance || n.Revoked || n.Desired != "running" {
					t.Error("busy failed prior desired restoration")
				}
			}
			if want == nil {
				if !op.Approved || op.Phase != "queued" || !n.Revoked || n.Desired != "terminating" {
					t.Error("idle delete not atomically approved")
				}
				if _, e = s.VerifyNodeToken(ctx, token); !errors.Is(e, model.ErrForbidden) {
					t.Error("approved delete credential not revoked", e)
				}
			}
			if n.DataVolume != "owned-data" {
				t.Error("authorization removed data")
			}
		})
	}
}

// Start must use the safe producer without destructive authorization I/O.
func TestOfflineDeleteUsesOwnedReportProof(t *testing.T) {
	for _, kind := range []string{"owned-zero", "unknown-zero", "busy-owned", "foreign-volume", "wrong-layout", "future", "running-node"} {
		t.Run(kind, func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := "task7-offline-" + f.UserID
			t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
			status := "missing"
			if kind == "running-node" {
				status = "running"
			}
			id := f.FleetNode(t, ns, testutil.Cols{"status": status, "desired": "stopped", "data_volume": "owned-data"})
			f.Cleanup(t, "DELETE FROM fleet_node_operations WHERE namespace=$1 AND owner_id=$2", ns, f.UserID)
			m := Maintainer{Repo: store.New(pool, ns), Diagnose: func(_ context.Context, n model.Node, _ model.OperationRef) (model.Observation, error) {
				o := model.Observation{Offline: true, Status: "missing", DataVolume: n.DataVolume, LayoutVersion: "1", ReportStatsKnown: true, ObservedAt: time.Now().UTC()}
				switch kind {
				case "unknown-zero":
					o.ReportStatsKnown = false
				case "busy-owned":
					o.PendingReports = 1
				case "foreign-volume":
					o.DataVolume = "foreign"
				case "wrong-layout":
					o.LayoutVersion = "0"
				case "future":
					o.ObservedAt = time.Now().Add(time.Hour)
				}
				return o, nil
			}}
			op, e := m.Request(context.Background(), util.MustParseUUID(f.UserID), util.MustParseUUID(id), model.Delete, "offline")
			want := model.ErrUnknownHealth
			if kind == "owned-zero" {
				want = nil
			}
			if kind == "busy-owned" {
				want = model.ErrBusy
			}
			if !errors.Is(e, want) {
				t.Fatalf("offline proof=%v want=%v", e, want)
			}
			if want == nil && !op.Approved {
				t.Error("owned report-zero did not approve")
			}
			if want == model.ErrUnknownHealth && op.Approved {
				t.Error("SQL idle substituted for report proof")
			}
		})
	}
}

func TestMaintenanceSecondFenceRechecks(t *testing.T) {
	for _, kind := range []string{"active-after-diagnosis", "epoch-after-diagnosis", "generation-after-diagnosis"} {
		t.Run(kind, func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := "task7-cas-" + f.UserID
			t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
			id := f.FleetNode(t, ns, testutil.Cols{"status": "running", "container_id": "current", "start_epoch": "epoch", "data_volume": "owned"})
			f.Cleanup(t, "DELETE FROM fleet_node_operations WHERE namespace=$1 AND owner_id=$2", ns, f.UserID)
			m := Maintainer{Repo: store.New(pool, ns), Diagnose: func(ctx context.Context, n model.Node, _ model.OperationRef) (model.Observation, error) {
				o := model.Observation{ContainerID: n.ContainerID, DaemonID: n.DaemonID, StartEpoch: n.StartEpoch, Ready: true, Status: "running", ReportStatsKnown: true, ObservedAt: time.Now().UTC()}
				switch kind {
				case "epoch-after-diagnosis":
					if _, e := pool.Exec(ctx, "UPDATE fleet_nodes SET start_epoch='newer' WHERE id=$1", id); e != nil {
						t.Fatal(e)
					}
				case "generation-after-diagnosis":
					if _, e := pool.Exec(ctx, "UPDATE fleet_nodes SET generation=generation+1 WHERE id=$1", id); e != nil {
						t.Fatal(e)
					}
				case "active-after-diagnosis":
					rt := f.Runtime(t, "managed", testutil.Cols{"metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, id))})
					ag := f.Agent(t, "accepted", rt)
					f.Task(t, ag, testutil.Cols{"runtime_id": rt, "status": "running"})
				}
				return o, nil
			}}
			op, e := m.Request(context.Background(), util.MustParseUUID(f.UserID), util.MustParseUUID(id), model.Stop, "second-fence")
			want := model.ErrUnknownHealth
			if kind == "active-after-diagnosis" {
				want = model.ErrBusy
			}
			if kind == "generation-after-diagnosis" {
				want = model.ErrConflict
			}
			if !errors.Is(e, want) || op.Approved {
				t.Fatalf("second fence approved changed snapshot=%+v err=%v", op, e)
			}
		})
	}
}

// A simulated API crash between phases leaves durable admission refusal, not physical permission.
func TestMaintenanceCrashBarrierAndSameOperationReview(t *testing.T) {
	for _, action := range []model.Action{model.Stop, model.Reboot, model.Delete} {
		t.Run(string(action), func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := "task7-crash-" + f.UserID
			t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
			id := f.FleetNode(t, ns, testutil.Cols{"status": "running", "container_id": "current", "start_epoch": "epoch", "data_volume": "owned"})
			f.Cleanup(t, "DELETE FROM fleet_node_operations WHERE namespace=$1 AND owner_id=$2", ns, f.UserID)
			rt := f.Runtime(t, "managed", testutil.Cols{"metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, id))})
			s := store.New(pool, ns)
			owner := util.MustParseUUID(f.UserID)
			node := util.MustParseUUID(id)
			ctx := context.Background()
			entered := make(chan struct{})
			release := make(chan struct{})
			m := Maintainer{Repo: s, Diagnose: func(context.Context, model.Node, model.OperationRef) (model.Observation, error) {
				close(entered)
				<-release
				return model.Observation{}, context.Canceled
			}}
			type result struct {
				op model.Operation
				e  error
			}
			done := make(chan result, 1)
			go func() { op, e := m.Request(ctx, owner, node, action, "crash"); done <- result{op, e} }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("prepare did not commit before diagnostic")
			}
			tx, e := pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			e = CheckClaim(ctx, db.New(tx), ns, util.MustParseUUID(rt), time.Now())
			_ = tx.Rollback(ctx)
			if !errors.Is(e, model.ErrBusy) {
				t.Error("claim escaped durable barrier", e)
			}
			tx, e = pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			e = CheckEnqueue(ctx, db.New(tx), ns, util.MustParseUUID(rt), time.Now())
			_ = tx.Rollback(ctx)
			if action == model.Delete && !errors.Is(e, model.ErrBusy) {
				t.Error("actual pending-delete consumer admitted", e)
			}
			if action != model.Delete && e != nil {
				t.Error("stop/reboot rejected ordinary queue", e)
			}
			close(release)
			got := <-done
			if !errors.Is(got.e, model.ErrUnknownHealth) {
				t.Fatal("crash unknown proof", got.e)
			}
			ref := model.OperationRef{Namespace: ns, NodeID: node, OperationID: got.op.ID, Generation: got.op.Generation, Action: action}
			controller := fleet.NewService(s, model.Config{Namespace: ns}, nil)
			if _, _, e = controller.AcceptOperation(ctx, owner, ref, "crash"); !errors.Is(e, model.ErrConflict) {
				t.Fatal("unapproved physical work accepted", e)
			}
			m.Diagnose = func(_ context.Context, n model.Node, _ model.OperationRef) (model.Observation, error) {
				return model.Observation{ContainerID: n.ContainerID, DaemonID: n.DaemonID, StartEpoch: n.StartEpoch, Ready: true, Status: "running", ReportStatsKnown: true, ObservedAt: time.Now().UTC()}, nil
			}
			approved, e := m.Review(ctx, owner, ref)
			if e != nil || approved.ID != got.op.ID || approved.Generation != got.op.Generation || !approved.Approved {
				t.Fatal("crash recovery changed operation", e)
			}
			replay, e := m.Review(ctx, owner, ref)
			if e != nil || replay.ID != approved.ID {
				t.Fatal("review replay changed operation", e)
			}
		})
	}
}

func TestFleetStartNoDestructiveAuthorization(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task7-start-guard-" + f.UserID
	t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
	id := f.FleetNode(t, ns, testutil.Cols{"status": "stopped", "desired": "stopped"})
	f.Cleanup(t, "DELETE FROM fleet_node_operations WHERE namespace=$1 AND owner_id=$2", ns, f.UserID)
	m := Maintainer{Repo: store.New(pool, ns), Diagnose: func(context.Context, model.Node, model.OperationRef) (model.Observation, error) {
		t.Fatal("start invoked destructive diagnostic")
		return model.Observation{}, nil
	}}
	op, e := m.Request(context.Background(), util.MustParseUUID(f.UserID), util.MustParseUUID(id), model.Start, "start")
	if e != nil || !op.Approved || op.Phase != "queued" {
		t.Fatal("safe start failed", e)
	}
}
