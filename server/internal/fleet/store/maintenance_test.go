package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"sync"
	"testing"
	"time"
)

// Generation/key serialization must preserve one operation and immutable provisioning data.
func TestFleetStartIntent(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task7-start-" + f.UserID
	t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
	cleanProduced(t, f, ns)
	id := f.FleetNode(t, ns, testutil.Cols{"status": "stopped", "desired": "stopped", "ready": false, "data_volume": "owned-data", "secrets_volume": "owned-secrets"})
	s := New(pool, ns)
	ctx := context.Background()
	owner := util.MustParseUUID(f.UserID)
	node := util.MustParseUUID(id)
	before, e := s.GetNode(ctx, owner, node)
	if e != nil {
		t.Fatal(e)
	}
	start := make(chan struct{})
	out := make(chan model.Operation, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			o, e := s.CreateStartIntent(ctx, owner, node, "same")
			out <- o
			errs <- e
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var opID pgtype.UUID
	for o := range out {
		if opID.Valid && o.ID != opID {
			t.Error("duplicate operation")
		}
		opID = o.ID
		if o.Phase != "queued" || !o.Approved || o.Generation != before.Generation+1 || o.Action != model.Start {
			t.Errorf("invalid start=%+v", o)
		}
	}
	after, e := s.GetNode(ctx, owner, node)
	if e != nil {
		t.Fatal(e)
	}
	if after.Desired != "running" || after.Ready || after.Maintenance || after.DataVolume != before.DataVolume || after.SecretsVolume != before.SecretsVolume || after.Resources != before.Resources || after.ProfileRef != before.ProfileRef || after.Image != before.Image {
		t.Errorf("start corrupted snapshot=%+v", after)
	}
	if _, e = s.PrepareMaintenance(ctx, owner, node, model.Stop, "same"); !errors.Is(e, model.ErrConflict) {
		t.Errorf("key/action conflict=%v", e)
	}
	if _, e = s.CreateStartIntent(ctx, owner, node, "other"); !errors.Is(e, model.ErrConflict) {
		t.Errorf("pending intent conflict=%v", e)
	}
	if _, e = New(pool, ns+"-foreign").CreateStartIntent(ctx, owner, node, "same"); !errors.Is(e, model.ErrForbidden) {
		t.Errorf("namespace escape=%v", e)
	}
	if _, e = s.CreateStartIntent(ctx, node, node, "same"); !errors.Is(e, model.ErrForbidden) {
		t.Errorf("owner escape=%v", e)
	}
}

func TestFleetApprovalRequiresExplicitCurrentProof(t *testing.T) {
	for _, kind := range []string{"trusted-current", "missing-baseline", "missing-observation", "foreign-owner", "foreign-namespace", "foreign-node", "foreign-operation", "stale-generation", "wrong-action", "altered-resource"} {
		t.Run(kind, func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := "task7-approve-" + f.UserID
			t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
			cleanProduced(t, f, ns)
			id := f.FleetNode(t, ns, testutil.Cols{"status": "running", "container_id": "current", "start_epoch": "epoch", "data_volume": "owned"})
			s := New(pool, ns)
			ctx := context.Background()
			owner := util.MustParseUUID(f.UserID)
			node := util.MustParseUUID(id)
			op, e := s.PrepareMaintenance(ctx, owner, node, model.Delete, "proof")
			if e != nil {
				t.Fatal(e)
			}
			baseline, e := s.GetNode(ctx, owner, node)
			if e != nil {
				t.Fatal(e)
			}
			ref := model.OperationRef{Namespace: ns, NodeID: node, OperationID: op.ID, Generation: op.Generation, Action: op.Action}
			observation := model.Observation{ContainerID: baseline.ContainerID, DaemonID: baseline.DaemonID, StartEpoch: baseline.StartEpoch, Status: "running", Ready: true, ReportStatsKnown: true, ObservedAt: time.Now().UTC()}
			requestOwner := owner
			want := model.ErrForbidden
			switch kind {
			case "trusted-current":
				want = nil
			case "missing-baseline":
				baseline = model.Node{}
				want = model.ErrUnknownHealth
			case "missing-observation":
				observation = model.Observation{}
				want = model.ErrUnknownHealth
			case "foreign-owner":
				requestOwner = node
			case "foreign-namespace":
				ref.Namespace += "-foreign"
			case "foreign-node":
				ref.NodeID = owner
			case "foreign-operation":
				ref.OperationID = owner
			case "stale-generation":
				ref.Generation++
				want = model.ErrConflict
			case "wrong-action":
				ref.Action = model.Stop
			case "altered-resource":
				baseline.Resources.CPUs++
				want = model.ErrUnknownHealth
			}
			e = s.ApproveMaintenance(ctx, requestOwner, ref, baseline, observation)
			if !errors.Is(e, want) {
				t.Fatalf("explicit approval=%v want=%v", e, want)
			}
			persisted, e := s.GetOperation(ctx, owner, op.ID)
			if e != nil {
				t.Fatal(e)
			}
			n, e := s.GetNode(ctx, owner, node)
			if e != nil {
				t.Fatal(e)
			}
			if kind == "trusted-current" {
				if !persisted.Approved || persisted.Phase != "queued" || !n.Revoked || n.Desired != "terminating" {
					t.Fatal("proved approval not atomic")
				}
			} else if persisted.Approved || persisted.Phase != "preparing" || !n.Maintenance || n.Revoked || n.Desired != "running" {
				t.Fatal("unproved approval lost durable barrier")
			}
		})
	}
}

// Active runs block every maintenance action; queues block only delete, without cancellation.
func TestFleetMaintenanceSQLGates(t *testing.T) {
	for _, action := range []model.Action{model.Stop, model.Reboot, model.Delete} {
		for _, state := range []string{"dispatched", "running", "waiting_local_directory", "queued", "deferred"} {
			t.Run(string(action)+"/"+state, func(t *testing.T) {
				pool, f := testutil.NewFleetFixture(t)
				ns := "task7-gate-" + f.UserID
				t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
				cleanProduced(t, f, ns)
				id := f.FleetNode(t, ns, testutil.Cols{"status": "running"})
				rt := f.Runtime(t, "managed", testutil.Cols{"metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, id))})
				ag := f.Agent(t, "retained", rt)
				task := f.Task(t, ag, testutil.Cols{"runtime_id": rt, "status": state})
				s := New(pool, ns)
				owner := util.MustParseUUID(f.UserID)
				node := util.MustParseUUID(id)
				ctx := context.Background()
				op, e := s.PrepareMaintenance(ctx, owner, node, action, "intent")
				blocked := state != "queued" && state != "deferred" || action == model.Delete
				if blocked {
					if !errors.Is(e, model.ErrBusy) {
						t.Fatalf("SQL gate=%v", e)
					}
				} else if e != nil || op.Phase != "preparing" || op.Approved {
					t.Fatalf("stop queue refused op=%+v e=%v", op, e)
				}
				n, e := s.GetNode(ctx, owner, node)
				if e != nil {
					t.Fatal(e)
				}
				if n.Maintenance == blocked || n.Desired != "running" || n.Revoked {
					t.Error("incorrect prepare barrier or early revoke")
				}
				var got string
				if e = pool.QueryRow(ctx, "SELECT status FROM agent_task_queue WHERE id=$1", task).Scan(&got); e != nil || got != state {
					t.Error("prepare changed business task", e)
				}
			})
		}
	}
}

func TestFleetMaintenanceAbortCAS(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task7-abort-" + f.UserID
	t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
	cleanProduced(t, f, ns)
	id := f.FleetNode(t, ns, testutil.Cols{"desired": "running", "status": "running"})
	s := New(pool, ns)
	ctx := context.Background()
	owner := util.MustParseUUID(f.UserID)
	node := util.MustParseUUID(id)
	token, credGen, e := s.MintNodeToken(ctx, node)
	if e != nil {
		t.Fatal(e)
	}
	op, e := s.PrepareMaintenance(ctx, owner, node, model.Delete, "delete")
	if e != nil {
		t.Fatal(e)
	}
	n, e := s.VerifyNodeToken(ctx, token)
	if e != nil || n.Desired != "running" || !n.Maintenance {
		t.Fatal("prepare lost accepted report grace", e)
	}
	if op.Generation == credGen {
		t.Log("generations coincidentally equal; separate credential value verified below")
	}
	var currentCred int64
	if e = pool.QueryRow(ctx, "SELECT max(generation) FROM fleet_node_credentials WHERE node_id=$1", id).Scan(&currentCred); e != nil || currentCred != credGen {
		t.Fatal("control advancement rotated credentials", e)
	}
	if e = s.AbortMaintenance(ctx, op.ID, op.Generation+1); !errors.Is(e, model.ErrConflict) {
		t.Error("stale abort allowed", e)
	}
	if _, e = s.CreateStartIntent(ctx, owner, node, "start"); !errors.Is(e, model.ErrConflict) {
		t.Error("start escaped prepare", e)
	}
	if e = s.AbortMaintenance(ctx, op.ID, op.Generation); e != nil {
		t.Fatal(e)
	}
	n, e = s.GetNode(ctx, owner, node)
	if e != nil || n.Maintenance || n.Desired != "running" || n.Revoked {
		t.Fatal("abort failed restoration", e)
	}
	failed, e := s.GetOperation(ctx, owner, op.ID)
	if e != nil || failed.Phase != "failed" || failed.Approved {
		t.Fatal("abort left pending intent", e)
	}
	if _, e = s.PrepareMaintenance(ctx, owner, node, model.Stop, "next"); e != nil {
		t.Fatal("failed operation kept admission", e)
	}
}
