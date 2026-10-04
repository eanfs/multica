package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A provider call under a DB lock deadlocks this owned lock acquisition; missing post-check accepts a stale CAS.
func TestServiceDiagnoseOutsideTransactionAndRechecksOperation(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "service-recheck-" + f.UserID
	id := f.FleetNode(t, ns, testutil.Cols{"container_id": "fake-container", "start_epoch": "epoch"})
	op := f.Insert(t, "fleet_node_operations", testutil.Cols{"namespace": ns, "owner_id": f.UserID, "node_id": id, "action": "stop", "idempotency_key": "recheck", "request_hash": "hash"})
	repo := store.New(pool, ns)
	ref := model.OperationRef{Namespace: ns, NodeID: mustUUID(t, id), OperationID: mustUUID(t, op), Generation: 1, Action: model.Stop}
	p := &fakeProvider{diagnose: func(ctx context.Context, n model.Node, _ model.OperationRef) (model.Observation, error) {
		lockCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if e := repo.WithTx(lockCtx, func(q *db.Queries) error {
			return q.FleetNodeExclusiveLock(lockCtx, db.FleetNodeExclusiveLockParams{Namespace: ns, NodeID: n.ID})
		}); e != nil {
			t.Errorf("provider called under DB lock: %v", e)
			return model.Observation{}, e
		}
		if _, e := pool.Exec(ctx, "UPDATE fleet_node_operations SET generation=2 WHERE id=$1", op); e != nil {
			t.Fatal(e)
		}
		return model.Observation{ContainerID: n.ContainerID, DaemonID: n.DaemonID, StartEpoch: n.StartEpoch, ObservedAt: time.Now(), ReportStatsKnown: true}, nil
	}}
	_, e := NewService(repo, model.Config{Namespace: ns}, p).Diagnose(context.Background(), mustUUID(t, f.UserID), ref)
	if !errors.Is(e, model.ErrConflict) {
		t.Fatalf("stale operation accepted: %v", e)
	}
}

// Unresponsive diagnosis is cancelled after the fixed five-second budget, not an arbitrary Docker timeout.
func TestServiceDiagnoseFixedFiveSecondBudget(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "service-budget-" + f.UserID
	id := f.FleetNode(t, ns)
	op := f.Insert(t, "fleet_node_operations", testutil.Cols{"namespace": ns, "owner_id": f.UserID, "node_id": id, "action": "stop", "idempotency_key": "budget", "request_hash": "hash"})
	ref := model.OperationRef{Namespace: ns, NodeID: mustUUID(t, id), OperationID: mustUUID(t, op), Generation: 1, Action: model.Stop}
	entered := false
	p := &fakeProvider{diagnose: func(ctx context.Context, _ model.Node, _ model.OperationRef) (model.Observation, error) {
		entered = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) < 4*time.Second {
			t.Error("not a fixed five-second budget")
		}
		<-ctx.Done()
		return model.Observation{}, ctx.Err()
	}}
	started := time.Now()
	_, e := NewService(store.New(pool, ns), model.Config{Namespace: ns}, p).Diagnose(context.Background(), mustUUID(t, f.UserID), ref)
	if !entered || !errors.Is(e, model.ErrUnavailable) || time.Since(started) > 6*time.Second || time.Since(started) < 4*time.Second {
		t.Fatalf("entered=%v err=%v elapsed=%v", entered, e, time.Since(started))
	}
}

// An offline proof for a replaced data volume must not survive the SQL recheck.
func TestServiceDiagnoseRechecksOfflineVolume(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "service-volume-" + f.UserID
	id := f.FleetNode(t, ns, testutil.Cols{"data_volume": "original-volume", "status": "stopped", "ready": false})
	op := f.Insert(t, "fleet_node_operations", testutil.Cols{"namespace": ns, "owner_id": f.UserID, "node_id": id, "action": "delete", "idempotency_key": "volume", "request_hash": "hash"})
	ref := model.OperationRef{Namespace: ns, NodeID: mustUUID(t, id), OperationID: mustUUID(t, op), Generation: 1, Action: model.Delete}
	p := &fakeProvider{diagnose: func(ctx context.Context, n model.Node, _ model.OperationRef) (model.Observation, error) {
		if _, e := pool.Exec(ctx, "UPDATE fleet_nodes SET data_volume='replaced-volume' WHERE id=$1", id); e != nil {
			t.Fatal(e)
		}
		return model.Observation{Offline: true, DataVolume: n.DataVolume, LayoutVersion: "1", ObservedAt: time.Now(), ReportStatsKnown: true}, nil
	}}
	_, e := NewService(store.New(pool, ns), model.Config{Namespace: ns}, p).Diagnose(context.Background(), mustUUID(t, f.UserID), ref)
	if !errors.Is(e, model.ErrConflict) {
		t.Fatalf("replaced volume proof accepted: %v", e)
	}
}
