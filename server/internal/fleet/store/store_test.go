package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func uuid(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	id, e := util.ParseUUID(s)
	if e != nil {
		t.Fatal(e)
	}
	return id
}

// Missing owner/namespace fences or fabricated timestamps leak rows or lose persisted time.
func TestFleetNodeOwnerIsolation(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := created.Add(time.Hour)
	id := f.FleetNode(t, "test-isolation", testutil.Cols{"created_at": created, "updated_at": updated})
	other := f.User(t, "other", "fleet-other-"+id+"@test.invalid")
	f.FleetNode(t, "test-isolation", testutil.Cols{"owner_id": other})
	f.FleetNode(t, "another-namespace")
	s := New(pool, "test-isolation")
	owner := uuid(t, f.UserID)
	nodes, err := s.ListNodes(context.Background(), owner, 20, 0)
	if err != nil || len(nodes) != 1 || util.UUIDToString(nodes[0].ID) != id {
		t.Fatalf("nodes=%v err=%v", nodes, err)
	}
	if !nodes[0].CreatedAt.Equal(created) || !nodes[0].UpdatedAt.Equal(updated) {
		t.Fatal("node persisted times not mapped")
	}
	n, err := s.GetNode(context.Background(), owner, uuid(t, id))
	if err != nil || n.ID != nodes[0].ID {
		t.Fatalf("get: %v", err)
	}
	if _, err = s.GetNode(context.Background(), uuid(t, other), uuid(t, id)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-owner: %v", err)
	}
	if _, err = New(pool, "wrong").GetNode(context.Background(), owner, uuid(t, id)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-namespace: %v", err)
	}
	page, err := s.ListNodes(context.Background(), owner, 1, 1)
	if err != nil || len(page) != 0 {
		t.Fatalf("pagination: %v %v", page, err)
	}
	opID := f.Insert(t, "fleet_node_operations", testutil.Cols{"namespace": "test-isolation", "owner_id": f.UserID, "node_id": id, "action": "start", "idempotency_key": "key", "request_hash": "hash", "created_at": created, "updated_at": updated})
	op, err := s.GetOperation(context.Background(), owner, uuid(t, opID))
	if err != nil || !op.CreatedAt.Equal(created) || !op.UpdatedAt.Equal(updated) {
		t.Fatalf("operation times: %v", err)
	}
	if _, err = s.GetOperation(context.Background(), uuid(t, other), uuid(t, opID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-owner operation: %v", err)
	}
}

func wantUnique(t *testing.T, err error) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23505" {
		t.Fatalf("want unique violation, got %v", err)
	}
}
func TestFleetIdentityAndIdempotency(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ctx := context.Background()
	id := f.FleetNode(t, "unique")
	_, err := pool.Exec(ctx, "INSERT INTO fleet_nodes(id,namespace,owner_id) VALUES($1,'different',$2)", id, f.UserID)
	wantUnique(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO fleet_nodes(namespace,owner_id,daemon_id) SELECT namespace,owner_id,daemon_id FROM fleet_nodes WHERE id=$1", id)
	wantUnique(t, err)
	op := f.Insert(t, "fleet_node_operations", testutil.Cols{"namespace": "unique", "owner_id": f.UserID, "node_id": id, "action": "start", "idempotency_key": "once", "request_hash": "h"})
	_, err = pool.Exec(ctx, "INSERT INTO fleet_node_operations(namespace,owner_id,node_id,action,idempotency_key,request_hash) SELECT namespace,owner_id,node_id,action,idempotency_key,request_hash FROM fleet_node_operations WHERE id=$1", op)
	wantUnique(t, err)
	f.Insert(t, "fleet_node_operations", testutil.Cols{"namespace": "different", "owner_id": f.UserID, "node_id": id, "action": "start", "idempotency_key": "once", "request_hash": "h"})
	c := f.Insert(t, "fleet_node_credentials", testutil.Cols{"namespace": "unique", "owner_id": f.UserID, "node_id": id, "token_hash": "test-only-hash-" + id})
	_, err = pool.Exec(ctx, "INSERT INTO fleet_node_credentials(namespace,owner_id,node_id,token_hash) SELECT namespace,owner_id,node_id,token_hash FROM fleet_node_credentials WHERE id=$1", c)
	wantUnique(t, err)
	f.Insert(t, "fleet_credential_profiles", testutil.Cols{"namespace": "unique", "owner_id": f.UserID, "profile_ref": "configured-reference"})
	_, err = pool.Exec(ctx, "INSERT INTO fleet_credential_profiles(namespace,owner_id,profile_ref) VALUES('unique',$1,'another')", f.UserID)
	wantUnique(t, err)
}

// Busy counts must include runs in other workspaces, but not another owner's runtime.
func TestFleetRuntimeAndRunCounts(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ctx := context.Background()
	id := f.FleetNode(t, "busy")
	owner := uuid(t, f.UserID)
	ws := f.Workspace(t, "other workspace", "fleet-"+id)
	f.Member(t, ws, f.UserID, "owner")
	for i, w := range []string{f.WorkspaceID, ws} {
		runtime := f.Runtime(t, "managed", testutil.Cols{"workspace_id": w, "runtime_mode": "local", "metadata": []byte("{\"managed_by\":\"local_fleet\",\"fleet_node_id\":\"" + id + "\"}")})
		agent := f.Agent(t, "fake", runtime, testutil.Cols{"workspace_id": w})
		f.Task(t, agent, testutil.Cols{"runtime_id": runtime, "status": "running"})
		f.Task(t, agent, testutil.Cols{"runtime_id": runtime, "status": "queued"})
		f.Task(t, agent, testutil.Cols{"runtime_id": runtime, "status": "deferred"})
		if i == 0 {
			q := db.New(pool)
			n, e := q.GetFleetNodeForRuntime(ctx, db.GetFleetNodeForRuntimeParams{Namespace: "busy", OwnerID: owner, RuntimeID: uuid(t, runtime)})
			if e != nil || util.UUIDToString(n.ID) != id {
				t.Fatalf("runtime association: %v", e)
			}
		}
	}
	malformed := f.Runtime(t, "malformed", testutil.Cols{"metadata": []byte("{\"managed_by\":\"local_fleet\",\"fleet_node_id\":\"not-a-uuid\"}")})
	other := f.User(t, "runtime other", "runtime-other-"+id+"@test.invalid")
	mismatch := f.Runtime(t, "wrong owner", testutil.Cols{"owner_id": other, "metadata": []byte("{\"managed_by\":\"local_fleet\",\"fleet_node_id\":\"" + id + "\"}")})
	agent := f.Agent(t, "wrong owner", mismatch, testutil.Cols{"owner_id": other})
	f.Task(t, agent, testutil.Cols{"runtime_id": mismatch, "status": "running"})
	f.Task(t, agent, testutil.Cols{"runtime_id": mismatch, "status": "queued"})
	for _, runtime := range []string{malformed, mismatch} {
		_, err := db.New(pool).GetFleetNodeForRuntime(ctx, db.GetFleetNodeForRuntimeParams{Namespace: "busy", OwnerID: owner, RuntimeID: uuid(t, runtime)})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid runtime association: %v", err)
		}
	}
	q := db.New(pool)
	active, e := q.CountFleetActiveRuns(ctx, db.CountFleetActiveRunsParams{Namespace: "busy", OwnerID: owner, NodeID: uuid(t, id)})
	if e != nil || active != 2 {
		t.Fatalf("active=%d err=%v", active, e)
	}
	queued, e := q.CountFleetQueuedRuns(ctx, db.CountFleetQueuedRunsParams{Namespace: "busy", OwnerID: owner, NodeID: uuid(t, id)})
	if e != nil || queued != 4 {
		t.Fatalf("queued=%d err=%v", queued, e)
	}
	count, err := q.CountFleetActiveRuns(ctx, db.CountFleetActiveRunsParams{Namespace: "other-namespace", OwnerID: owner, NodeID: uuid(t, id)})
	if err != nil || count != 0 {
		t.Fatalf("cross-namespace active=%d err=%v", count, err)
	}
}

// Lock waits and idle callbacks cannot hold a store transaction indefinitely; errors roll back.
func TestFleetTransactionBoundsAndRollback(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	id := f.FleetNode(t, "locks")
	s := New(pool, "locks")
	ctx := context.Background()
	blocker, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	q := db.New(blocker)
	if e = q.FleetNodeExclusiveLock(ctx, db.FleetNodeExclusiveLockParams{Namespace: "locks", NodeID: uuid(t, id)}); e != nil {
		t.Fatal(e)
	}
	started := time.Now()
	e = s.WithTx(ctx, func(q *db.Queries) error {
		return q.FleetNodeSharedLock(ctx, db.FleetNodeSharedLockParams{Namespace: "locks", NodeID: uuid(t, id)})
	})
	if e == nil || time.Since(started) > 4*time.Second {
		t.Fatalf("unbounded lock: %v", e)
	}
	sentinel := errors.New("rollback")
	e = s.WithTx(ctx, func(q *db.Queries) error {
		if e := q.FleetNodeCapacityLock(ctx, db.FleetNodeCapacityLockParams{Namespace: "locks", NodeID: uuid(t, id)}); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(e, sentinel) {
		t.Fatalf("callback: %v", e)
	}
	// Rollback must release the capacity lock, not just return the callback error.
	if e = s.WithTx(ctx, func(q *db.Queries) error {
		return q.FleetNodeCapacityLock(ctx, db.FleetNodeCapacityLockParams{Namespace: "locks", NodeID: uuid(t, id)})
	}); e != nil {
		t.Fatalf("rollback retained lock: %v", e)
	}
}

// Shared locks permit concurrent admissions; another namespace must never collide.
func TestFleetSharedLocksAndNamespace(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	id := f.FleetNode(t, "shared")
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = db.New(tx).FleetNodeSharedLock(ctx, db.FleetNodeSharedLockParams{Namespace: "shared", NodeID: uuid(t, id)}); err != nil {
		t.Fatal(err)
	}
	if err = New(pool, "shared").WithTx(ctx, func(q *db.Queries) error {
		return q.FleetNodeSharedLock(ctx, db.FleetNodeSharedLockParams{Namespace: "shared", NodeID: uuid(t, id)})
	}); err != nil {
		t.Fatalf("shared admissions blocked: %v", err)
	}
	if err = New(pool, "other").WithTx(ctx, func(q *db.Queries) error {
		return q.FleetNodeExclusiveLock(ctx, db.FleetNodeExclusiveLockParams{Namespace: "other", NodeID: uuid(t, id)})
	}); err != nil {
		t.Fatalf("namespace locks collided: %v", err)
	}
}

// Repeated short queries must not extend the server-side transaction beyond two seconds.
func TestFleetTransactionTotalTimeout(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	id := f.FleetNode(t, "total-timeout")
	s := New(pool, "total-timeout")
	var lateErr error
	_ = s.WithTx(context.Background(), func(q *db.Queries) error {
		for i := 0; i < 2; i++ {
			<-time.After(1100 * time.Millisecond)
			lateErr = q.FleetNodeCapacityLock(context.Background(), db.FleetNodeCapacityLockParams{Namespace: "total-timeout", NodeID: uuid(t, id)})
			if lateErr != nil {
				return lateErr
			}
		}
		return nil
	})
	if lateErr == nil {
		t.Fatal("transaction accepted database work after its two-second budget")
	}
}
