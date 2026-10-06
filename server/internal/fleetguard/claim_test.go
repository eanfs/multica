package fleetguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"testing"
	"time"
)

func TestNamespaceSQLClaimReclaimEnqueueAndOrdinary(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task1-guard-" + f.UserID
	ctx := context.Background()
	f.Cleanup(t, "DELETE FROM fleet_namespace_fences WHERE namespace=$1", ns)
	node := f.FleetNode(t, ns, testutil.Cols{"status": "running"})
	rt := f.Runtime(t, "managed", testutil.Cols{"metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	ordinary := f.Runtime(t, "ordinary")
	if _, err := store.New(pool, ns).CloseNamespace(ctx, "fixture-fleet", "close"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"claim", "reclaim", "enqueue"} {
		t.Run(kind, func(t *testing.T) {
			tx, e := pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			q := db.New(tx)
			id := util.MustParseUUID(rt)
			switch kind {
			case "claim":
				e = CheckClaim(ctx, q, ns, id, time.Now())
			case "reclaim":
				e = CheckReclaim(ctx, q, ns, []pgtype.UUID{id}, time.Now())
			case "enqueue":
				e = CheckEnqueue(ctx, q, ns, id, time.Now())
			}
			if !errors.Is(e, model.ErrBusy) {
				t.Fatalf("%s crossed namespace fence: %v", kind, e)
			}
		})
	}
	registerTx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = CheckRegister(ctx, db.New(registerTx), ns, util.MustParseUUID(node)); e != nil {
		t.Errorf("already-admitted bootstrap registration denied: %v", e)
	}
	if e = registerTx.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	ids, _, e := ReclaimCandidates(ctx, db.New(tx), []pgtype.UUID{util.MustParseUUID(rt), util.MustParseUUID(ordinary)}, time.Now())
	if e != nil || len(ids) != 1 || ids[0] != util.MustParseUUID(ordinary) {
		t.Fatalf("closed managed contaminated ordinary reclaim: %v %v", ids, e)
	}
}

func TestClaimBarrierSeesMaintenance(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task6-guard-" + uuid.NewString()
	node := f.FleetNode(t, ns, testutil.Cols{"status": "running", "maintenance": true})
	rt := f.Runtime(t, "guard", testutil.Cols{"metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := CheckClaim(context.Background(), db.New(tx), ns, util.MustParseUUID(rt), time.Now()); !errors.Is(err, model.ErrBusy) {
		t.Fatalf("maintenance error=%v", err)
	}
}

func TestClaimBarrierNamespaceOwnerAndMalformedNode(t *testing.T) {
	for _, kind := range []string{"namespace", "missing-node", "invalid-id", "bad-marker", "wrong-owner"} {
		t.Run(kind, func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := "task6-identity-" + uuid.NewString()
			node := f.FleetNode(t, ns, testutil.Cols{"status": "running"})
			marker := "local_fleet"
			owner := f.UserID
			wantNS := ns
			switch kind {
			case "namespace":
				wantNS = "wrong"
			case "missing-node":
				node = uuid.NewString()
			case "invalid-id":
				node = "bad"
			case "bad-marker":
				marker = "forged"
			case "wrong-owner":
				owner = f.User(t, "other", "other-"+uuid.NewString()+"@test.invalid")
			}
			rt := f.Runtime(t, "guard", testutil.Cols{"owner_id": owner, "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"%s","fleet_node_id":"%s"}`, marker, node))})
			tx, err := pool.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if err := CheckClaim(context.Background(), db.New(tx), wantNS, util.MustParseUUID(rt), time.Now()); err == nil {
				t.Fatal("invalid managed identity downgraded to ordinary")
			}
		})
	}
}

func TestClaimBarrierPendingReportsAndOwnPreparationSlot(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task6-preparation-" + uuid.NewString()
	node := f.FleetNode(t, ns, testutil.Cols{"status": "running", "pending_reports": 4, "failed_reports": 3})
	rt := f.Runtime(t, "guard", testutil.Cols{"metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	rid := util.MustParseUUID(rt)
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckClaim(context.Background(), db.New(tx), ns, rid, time.Now()); err != nil {
		t.Fatalf("pending reports blocked ordinary claim: %v", err)
	}
	_ = tx.Rollback(context.Background())
	ag := f.Agent(t, "prep", rt)
	f.Task(t, ag, testutil.Cols{"runtime_id": rt, "status": "dispatched", "dispatched_at": time.Now(), "prepare_lease_expires_at": time.Now().Add(time.Minute)})
	tx, err = pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := CheckClaim(context.Background(), db.New(tx), ns, rid, time.Now()); !errors.Is(err, model.ErrBusy) {
		t.Fatalf("preparation slot not counted: %v", err)
	}
	if err := CheckReclaim(context.Background(), db.New(tx), ns, []pgtype.UUID{rid}, time.Now()); err != nil {
		t.Fatalf("own dispatched slot counted twice: %v", err)
	}
}

func TestFleetPendingDeleteAdmissionUsesCurrentScopedOperation(t *testing.T) {
	for _, tc := range []struct {
		name, phase, action string
		generation          int64
		wrongNamespace      bool
		blocked             bool
	}{
		{"queued", "queued", "delete", 1, false, true}, {"preparing", "preparing", "delete", 1, false, true}, {"prepared", "prepared", "delete", 1, false, true}, {"applying", "applying", "delete", 1, false, true},
		{"completed", "completed", "delete", 1, false, false}, {"failed", "failed", "delete", 1, false, false}, {"aborted", "aborted", "delete", 1, false, false},
		{"old-generation", "preparing", "delete", 2, false, false}, {"other-namespace", "preparing", "delete", 1, true, false}, {"stop-retains-queue", "preparing", "stop", 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := "task6-fix-op-" + uuid.NewString()
			t.Logf("Task6FixScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
			node := f.FleetNode(t, ns, testutil.Cols{"maintenance": true, "status": "running", "desired": "running"})
			operationNS := ns
			if tc.wrongNamespace {
				operationNS += "-other"
			}
			f.Insert(t, "fleet_node_operations", testutil.Cols{"namespace": operationNS, "owner_id": f.UserID, "node_id": node, "action": tc.action, "phase": tc.phase, "generation": tc.generation, "idempotency_key": "fixture", "request_hash": "fixture", "prior_desired": "running"})
			rt := f.Runtime(t, "op", testutil.Cols{"metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
			tx, e := pool.Begin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			e = CheckEnqueue(context.Background(), db.New(tx), ns, util.MustParseUUID(rt), time.Now())
			if tc.blocked && !errors.Is(e, model.ErrBusy) {
				t.Fatalf("current pending delete admitted: %v", e)
			}
			if !tc.blocked && e != nil {
				t.Fatalf("unrelated or terminal operation blocked enqueue: %v", e)
			}
		})
	}
}

func TestCallerEnqueueRequiresActualOwningTransactionLocks(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ctx := context.Background()
	ns := "task6-caller-" + uuid.NewString()
	node := f.FleetNode(t, ns)
	rt := f.Runtime(t, "caller", testutil.Cols{"metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	if err := CheckCallerEnqueue(ctx, q, util.MustParseUUID(rt), time.Now()); !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("unguarded caller admitted: %v", err)
	}
	if err := CheckEnqueue(ctx, q, ns, util.MustParseUUID(rt), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := CheckCallerEnqueue(ctx, q, util.MustParseUUID(rt), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Rollback(ctx)
	if err := CheckCallerEnqueue(ctx, db.New(next), util.MustParseUUID(rt), time.Now()); !errors.Is(err, ErrBindingChanged) {
		t.Fatalf("old transaction cached an admission: %v", err)
	}
}
