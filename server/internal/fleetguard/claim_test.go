package fleetguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"testing"
	"time"
)

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
