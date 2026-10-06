package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"strings"
)

// A missing node-wide transaction gate dispatches both agents despite MaxRuns=1.
func TestFleetClaimCrossWorkspaceCapacity(t *testing.T) {
	for _, maxRuns := range []int{1, 2} {
		t.Run(fmt.Sprintf("max_runs_%d", maxRuns), func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := ownedFleetNamespace(t, pool, f, "task6-capacity-")
			node := f.FleetNode(t, ns, testutil.Cols{"status": "running", "spec_config": json.RawMessage(fmt.Sprintf(`{"cpus":2,"memory_bytes":4294967296,"pids":256,"max_runs":%d}`, maxRuns))})
			second := f.Workspace(t, "fleet second", "fleet-second-"+uuid.NewString())
			f.Member(t, second, f.UserID, "owner")
			runtimes := make([]pgtype.UUID, 2)
			for i, ws := range []string{f.WorkspaceID, second} {
				rt := f.Runtime(t, fmt.Sprintf("fleet-%d", i), testutil.Cols{"workspace_id": ws, "runtime_mode": "local", "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
				ag := f.Agent(t, fmt.Sprintf("fleet-%d", i), rt, testutil.Cols{"workspace_id": ws, "runtime_mode": "local"})
				f.Task(t, ag, testutil.Cols{"runtime_id": rt})
				runtimes[i] = util.MustParseUUID(rt)
			}
			svc := NewTaskService(db.New(pool), pool, nil, events.New())
			start := make(chan struct{})
			results := make(chan *db.AgentTaskQueue, 2)
			errs := make(chan error, 2)
			var wg sync.WaitGroup
			for _, rt := range runtimes {
				wg.Add(1)
				go func(id pgtype.UUID) {
					defer wg.Done()
					<-start
					row, err := svc.ClaimTaskForRuntime(context.Background(), id)
					results <- row
					errs <- err
				}(rt)
			}
			close(start)
			wg.Wait()
			close(results)
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			count := 0
			for row := range results {
				if row != nil {
					count++
				}
			}
			if count != maxRuns {
				t.Fatalf("node max_runs=1 dispatched %d tasks across workspaces", count)
			}
			var active int
			if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM agent_task_queue WHERE runtime_id=ANY($1::uuid[]) AND status='dispatched'", runtimes).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if active != maxRuns {
				t.Fatalf("persisted active=%d", active)
			}
		})
	}
}

func TestFleetClaimBarrierAllServicePaths(t *testing.T) {
	for _, path := range []string{"legacy", "single", "batch", "single-reclaim", "batch-reclaim"} {
		for _, gate := range []string{"maintenance", "revoked", "stale", "unknown", "not-ready", "terminating"} {
			t.Run(path+"/"+gate, func(t *testing.T) {
				pool, f := testutil.NewFleetFixture(t)
				ns := ownedFleetNamespace(t, pool, f, "task6-barrier-")
				cols := testutil.Cols{"status": "running"}
				switch gate {
				case "maintenance":
					cols["maintenance"] = true
				case "revoked":
					cols["revoked"] = true
				case "stale":
					cols["health_at"] = time.Now().Add(-claimResponseRecoveryWindow - time.Second)
				case "unknown":
					cols["health_at"] = nil
				case "not-ready":
					cols["ready"] = false
				case "terminating":
					cols["desired"] = "terminating"
				}
				node := f.FleetNode(t, ns, cols)
				rt := f.Runtime(t, "barrier", testutil.Cols{"runtime_mode": "local", "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
				ag := f.Agent(t, "barrier", rt, testutil.Cols{"runtime_mode": "local"})
				tc := testutil.Cols{"runtime_id": rt}
				if path == "single-reclaim" || path == "batch-reclaim" {
					tc["status"] = "dispatched"
					tc["dispatched_at"] = time.Now().Add(-claimResponseRecoveryWindow - time.Second)
					tc["prepare_lease_expires_at"] = time.Now().Add(-claimResponseRecoveryWindow - time.Second)
				}
				id := f.Task(t, ag, tc)
				before, err := db.New(pool).GetAgentTask(context.Background(), util.MustParseUUID(id))
				if err != nil {
					t.Fatal(err)
				}
				svc := NewTaskService(db.New(pool), pool, nil, events.New())
				var rows []*db.AgentTaskQueue
				switch path {
				case "legacy":
					row, _ := svc.ClaimTask(context.Background(), util.MustParseUUID(ag))
					rows = append(rows, row)
				case "single", "single-reclaim":
					row, _ := svc.ClaimTaskForRuntime(context.Background(), util.MustParseUUID(rt))
					rows = append(rows, row)
				default:
					batch, _ := svc.ClaimTasksForRuntimes(context.Background(), []pgtype.UUID{util.MustParseUUID(rt)}, 10)
					for i := range batch {
						rows = append(rows, &batch[i])
					}
				}
				for _, row := range rows {
					if row != nil {
						t.Fatalf("%s passed %s barrier: task=%s", path, gate, util.UUIDToString(row.ID))
					}
				}
				after, err := db.New(pool).GetAgentTask(context.Background(), util.MustParseUUID(id))
				if err != nil {
					t.Fatal(err)
				}
				if after.Status != before.Status || after.DispatchedAt != before.DispatchedAt || after.PrepareLeaseExpiresAt != before.PrepareLeaseExpiresAt {
					t.Fatalf("barrier changed task lease/status")
				}
			})
		}
	}
}

// These hooks expose query-entry barriers, not cached admission permissions.
type fleetClaimTxStarter struct {
	pool                *pgxpool.Pool
	entered             chan struct{}
	once                sync.Once
	rowHook             func(context.Context, string, pgx.Row) pgx.Row
	beforeRow           func(context.Context, string)
	unlimitedStatements bool
}

func (s *fleetClaimTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if s.unlimitedStatements {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout=0; SET LOCAL lock_timeout=0"); err != nil {
			_ = tx.Rollback(context.Background())
			return nil, err
		}
	}
	return &fleetClaimTx{Tx: tx, starter: s}, nil
}

type fleetClaimTx struct {
	pgx.Tx
	starter *fleetClaimTxStarter
}

func (t *fleetClaimTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "name: FleetNodeSharedLock") {
		t.starter.once.Do(func() { close(t.starter.entered) })
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func (t *fleetClaimTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if t.starter.beforeRow != nil {
		t.starter.beforeRow(ctx, sql)
	}
	row := t.Tx.QueryRow(ctx, sql, args...)
	if t.starter.rowHook != nil {
		return t.starter.rowHook(ctx, sql, row)
	}
	return row
}

type fleetPausedRow struct {
	pgx.Row
	after func()
}

func (r fleetPausedRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	if err == nil {
		r.after()
	}
	return err
}

func TestFleetClaimMaintenanceRace(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := ownedFleetNamespace(t, pool, f, "task6-race-")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node := f.FleetNode(t, ns, testutil.Cols{"status": "running"})
	rt := f.Runtime(t, "race", testutil.Cols{"runtime_mode": "local", "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	ag := f.Agent(t, "race", rt, testutil.Cols{"runtime_mode": "local"})
	id := f.Task(t, ag, testutil.Cols{"runtime_id": rt})
	maint, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer maint.Rollback(context.Background())
	q := db.New(maint)
	if err := q.FleetNodeExclusiveLock(ctx, db.FleetNodeExclusiveLockParams{Namespace: ns, NodeID: util.MustParseUUID(node)}); err != nil {
		t.Fatal(err)
	}
	if _, err := maint.Exec(ctx, "UPDATE fleet_nodes SET maintenance=true WHERE id=$1 AND namespace=$2", node, ns); err != nil {
		t.Fatal(err)
	}
	starter := &fleetClaimTxStarter{pool: pool, entered: make(chan struct{})}
	svc := NewTaskService(db.New(pool), starter, nil, events.New())
	type result struct {
		task *db.AgentTaskQueue
		err  error
	}
	done := make(chan result, 1)
	go func() { task, err := svc.ClaimTask(ctx, util.MustParseUUID(ag)); done <- result{task, err} }()
	select {
	case <-starter.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// A claim waiting at the node gate must NOT already own workspace/runtime rows.
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Exec(ctx, "SELECT id FROM workspace WHERE id=$1 FOR UPDATE NOWAIT", f.WorkspaceID); err != nil {
		t.Fatalf("workspace locked before node: %v", err)
	}
	if _, err := probe.Exec(ctx, "SELECT id FROM agent_runtime WHERE id=$1 FOR UPDATE NOWAIT", rt); err != nil {
		t.Fatalf("runtime locked before node: %v", err)
	}
	_ = probe.Rollback(ctx)
	if err := maint.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.task != nil {
			t.Fatalf("maintenance race escaped: task=%v err=%v", r.task, r.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	row, err := db.New(pool).GetAgentTask(ctx, util.MustParseUUID(id))
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "queued" {
		t.Fatalf("race dispatched %s", row.Status)
	}
}

func TestFleetClaimBindingRaceRetriesWholeTransaction(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := ownedFleetNamespace(t, pool, f, "task6-rebind-")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	oldNode := f.FleetNode(t, ns, testutil.Cols{"status": "running"})
	newNode := f.FleetNode(t, ns, testutil.Cols{"status": "running"})
	mkRuntime := func(name, node string) string {
		return f.Runtime(t, name, testutil.Cols{"runtime_mode": "local", "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	}
	oldRT, newRT := mkRuntime("old", oldNode), mkRuntime("new", newNode)
	ag := f.Agent(t, "rebind", oldRT, testutil.Cols{"runtime_mode": "local"})
	id := f.Task(t, ag, testutil.Cols{"runtime_id": oldRT})
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if err := db.New(blocker).FleetNodeExclusiveLock(ctx, db.FleetNodeExclusiveLockParams{Namespace: ns, NodeID: util.MustParseUUID(oldNode)}); err != nil {
		t.Fatal(err)
	}
	starter := &fleetClaimTxStarter{pool: pool, entered: make(chan struct{})}
	svc := NewTaskService(db.New(pool), starter, nil, events.New())
	done := make(chan error, 1)
	go func() {
		row, e := svc.ClaimTask(ctx, util.MustParseUUID(ag))
		if e == nil && (row == nil || row.ID != util.MustParseUUID(id) || row.RuntimeID != util.MustParseUUID(newRT)) {
			e = fmt.Errorf("claimed stale binding: %v", row)
		}
		done <- e
	}()
	select {
	case <-starter.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := pool.Exec(ctx, "UPDATE agent SET runtime_id=$1 WHERE id=$2", newRT, ag); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE agent_task_queue SET runtime_id=$1 WHERE id=$2", newRT, id); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	oldActive, err := db.New(pool).CountFleetActiveRuns(ctx, db.CountFleetActiveRunsParams{Namespace: ns, OwnerID: util.MustParseUUID(f.UserID), NodeID: util.MustParseUUID(oldNode)})
	if err != nil {
		t.Fatal(err)
	}
	newActive, err := db.New(pool).CountFleetActiveRuns(ctx, db.CountFleetActiveRunsParams{Namespace: ns, OwnerID: util.MustParseUUID(f.UserID), NodeID: util.MustParseUUID(newNode)})
	if err != nil {
		t.Fatal(err)
	}
	if oldActive != 0 || newActive != 1 {
		t.Fatalf("binding capacities old=%d new=%d", oldActive, newActive)
	}
}

func TestFleetBatchReclaimDoesNotBlockOrdinaryRuntime(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := ownedFleetNamespace(t, pool, f, "task6-mixed-")
	node := f.FleetNode(t, ns, testutil.Cols{"status": "running", "maintenance": true})
	managed := f.Runtime(t, "managed", testutil.Cols{"runtime_mode": "local", "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	ordinary := f.Runtime(t, "ordinary")
	var ids []pgtype.UUID
	var want pgtype.UUID
	for _, rt := range []string{managed, ordinary} {
		ag := f.Agent(t, "mixed-"+rt, rt, testutil.Cols{"runtime_mode": "local"})
		id := f.Task(t, ag, testutil.Cols{"runtime_id": rt, "status": "dispatched", "dispatched_at": time.Now().Add(-claimResponseRecoveryWindow - time.Second), "prepare_lease_expires_at": time.Now().Add(-claimResponseRecoveryWindow - time.Second)})
		ids = append(ids, util.MustParseUUID(rt))
		if rt == ordinary {
			want = util.MustParseUUID(id)
		}
	}
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	rows, err := svc.ClaimTasksForRuntimes(context.Background(), ids, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != want {
		t.Fatalf("managed maintenance affected ordinary redelivery: rows=%v err=%v", rows, err)
	}
}

func TestFleetClaimNoAutocommitAdmission(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := ownedFleetNamespace(t, pool, f, "task6-niltx-")
	node := f.FleetNode(t, ns, testutil.Cols{"status": "running"})
	rt := f.Runtime(t, "niltx", testutil.Cols{"runtime_mode": "local", "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	ag := f.Agent(t, "niltx", rt, testutil.Cols{"runtime_mode": "local"})
	id := f.Task(t, ag, testutil.Cols{"runtime_id": rt})
	svc := NewTaskService(db.New(pool), nil, nil, events.New())
	row, err := svc.ClaimTask(context.Background(), util.MustParseUUID(ag))
	if err == nil || row != nil {
		t.Fatalf("managed autocommit claim allowed: %v %v", row, err)
	}
	after, err := db.New(pool).GetAgentTask(context.Background(), util.MustParseUUID(id))
	if err != nil || after.Status != "queued" {
		t.Fatalf("autocommit mutated task: %v %v", after, err)
	}
	ordinary := f.Runtime(t, "ordinary-niltx")
	ordinaryAgent := f.Agent(t, "ordinary-niltx", ordinary, testutil.Cols{"runtime_mode": "local"})
	ordinaryID := f.Task(t, ordinaryAgent, testutil.Cols{"runtime_id": ordinary})
	row, err = svc.ClaimTask(context.Background(), util.MustParseUUID(ordinaryAgent))
	if err != nil || row == nil || row.ID != util.MustParseUUID(ordinaryID) {
		t.Fatalf("ordinary nil starter fallback changed: row=%v err=%v", row, err)
	}
}

func TestFleetOppositeBatchReclaimsUseSortedNodeLocks(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task6-sorted-" + uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ids := make([]pgtype.UUID, 0, 2)
	want := map[pgtype.UUID]bool{}
	for i := 0; i < 2; i++ {
		node := f.FleetNode(t, ns, testutil.Cols{"status": "running"})
		rt := f.Runtime(t, fmt.Sprintf("sorted-%d", i), testutil.Cols{"runtime_mode": "local", "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
		ag := f.Agent(t, fmt.Sprintf("sorted-%d", i), rt, testutil.Cols{"runtime_mode": "local"})
		task := f.Task(t, ag, testutil.Cols{"runtime_id": rt, "status": "dispatched", "dispatched_at": time.Now().Add(-claimResponseRecoveryWindow - time.Second), "prepare_lease_expires_at": time.Now().Add(-time.Second)})
		ids = append(ids, util.MustParseUUID(rt))
		want[util.MustParseUUID(task)] = true
	}
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	start := make(chan struct{})
	type result struct {
		rows []db.AgentTaskQueue
		err  error
	}
	done := make(chan result, 2)
	for _, order := range [][]pgtype.UUID{ids, {ids[1], ids[0]}} {
		go func(order []pgtype.UUID) {
			<-start
			rows, err := svc.ClaimTasksForRuntimes(ctx, order, 2)
			done <- result{rows, err}
		}(order)
	}
	close(start)
	seen := map[pgtype.UUID]bool{}
	for i := 0; i < 2; i++ {
		select {
		case result := <-done:
			if result.err != nil {
				t.Fatal(result.err)
			}
			for _, row := range result.rows {
				if !want[row.ID] || seen[row.ID] {
					t.Fatal("stale dispatched task redelivered twice")
				}
				seen[row.ID] = true
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if len(seen) != 2 {
		t.Fatalf("redelivered=%d want two own preparation slots", len(seen))
	}
}

// Admission must not retain node/capacity locks while an owner row waits forever.
func TestFleetManagedAttemptBoundsPostAdmissionOwnerLock(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := ownedFleetNamespace(t, pool, f, "task6-fix-budget-")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	node := f.FleetNode(t, ns, testutil.Cols{"status": "running"})
	rt := f.Runtime(t, "budget", testutil.Cols{"runtime_mode": "local", "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	ag := f.Agent(t, "budget", rt, testutil.Cols{"runtime_mode": "local"})
	task := f.Task(t, ag, testutil.Cols{"runtime_id": rt})
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, "SELECT id FROM agent WHERE id=$1 FOR UPDATE", ag); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var once sync.Once
	starter := &fleetClaimTxStarter{pool: pool, unlimitedStatements: true, entered: make(chan struct{}), beforeRow: func(c context.Context, sql string) {
		if strings.Contains(sql, "name: GetAgentForClaimUpdate") {
			once.Do(func() { close(entered) })
		}
	}}
	svc := NewTaskService(db.New(pool), starter, nil, events.New())
	done := make(chan error, 1)
	started := time.Now()
	go func() { _, e := svc.ClaimTask(ctx, util.MustParseUUID(ag)); done <- e }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	timer := time.NewTimer(2500 * time.Millisecond)
	defer timer.Stop()
	timedOut := false
	select {
	case err := <-done:
		if err == nil {
			t.Error("blocked owner claim returned success")
		}
	case <-timer.C:
		timedOut = true
		t.Error("managed attempt exceeded two-second budget after admission")
	}
	// On both RED and GREEN release blockers and collect the goroutine before cleanup.
	_ = blocker.Rollback(context.Background())
	if timedOut {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if !timedOut && time.Since(started) > 2500*time.Millisecond {
		t.Error("late attempt rollback")
	}
	proof, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer proof.Rollback(context.Background())
	proofCtx, stopProof := context.WithDeadline(ctx, started.Add(2500*time.Millisecond))
	defer stopProof()
	for _, key := range []string{ns + ":" + node, ns + ":" + node + ":capacity"} {
		if _, err = proof.Exec(proofCtx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", key); err != nil {
			t.Fatalf("managed rollback retained admission domain within budget: %v", err)
		}
	}
	row, err := db.New(pool).GetAgentTask(ctx, util.MustParseUUID(task))
	if err != nil {
		t.Fatal(err)
	}
	if !timedOut && row.Status != "queued" {
		t.Fatalf("timeout mutated task: %s", row.Status)
	}
}

func TestFleetOrdinaryDiscoveryTrustedUpgradeRetriesBeforeNodeLocks(t *testing.T) {
	for _, path := range []string{"claim", "enqueue"} {
		t.Run(path, func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := ownedFleetNamespace(t, pool, f, "task6-fix-upgrade-")
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			node := f.FleetNode(t, ns, testutil.Cols{"status": "running", "maintenance": true})
			if path == "enqueue" {
				if _, e := pool.Exec(ctx, "UPDATE fleet_nodes SET desired='terminating' WHERE id=$1 AND namespace=$2", node, ns); e != nil {
					t.Fatal(e)
				}
			}
			daemon := uuid.NewString()
			rt := f.Runtime(t, "upgrade", testutil.Cols{"runtime_mode": "local", "daemon_id": daemon, "provider": "claude"})
			ag := f.Agent(t, "upgrade", rt, testutil.Cols{"runtime_mode": "local"})
			task := f.Task(t, ag, testutil.Cols{"runtime_id": rt})
			f.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", ag)
			paused, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			starter := &fleetClaimTxStarter{pool: pool, unlimitedStatements: true, entered: make(chan struct{}), rowHook: func(c context.Context, sql string, row pgx.Row) pgx.Row {
				if strings.Contains(sql, "name: GetAgentRuntime :one") {
					return fleetPausedRow{Row: row, after: func() {
						once.Do(func() {
							close(paused)
							select {
							case <-resume:
							case <-c.Done():
							}
						})
					}}
				}
				return row
			}}
			svc := NewTaskService(db.New(pool), starter, nil, events.New())
			done := make(chan error, 1)
			go func() {
				if path == "claim" {
					row, e := svc.ClaimTask(ctx, util.MustParseUUID(ag))
					if row != nil {
						e = fmt.Errorf("upgraded maintenance runtime claimed queued task")
					}
					done <- e
				} else {
					_, e := svc.EnqueueQuickCreateTask(ctx, util.MustParseUUID(f.WorkspaceID), util.MustParseUUID(f.UserID), util.MustParseUUID(ag), pgtype.UUID{}, "fixture", "high", "", pgtype.UUID{}, pgtype.UUID{}, nil)
					done <- e
				}
			}()
			select {
			case <-paused:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// Actual trusted same-owner SQL upsert, not arbitrary metadata UPDATE.
			upgraded, err := db.New(pool).UpsertAgentRuntime(ctx, db.UpsertAgentRuntimeParams{WorkspaceID: util.MustParseUUID(f.WorkspaceID), DaemonID: pgtype.Text{String: daemon, Valid: true}, Name: "upgrade", RuntimeMode: "local", Provider: "claude", Status: "online", OwnerID: util.MustParseUUID(f.UserID), Metadata: json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
			if err != nil {
				close(resume)
				t.Fatal(err)
			}
			if upgraded.ID != util.MustParseUUID(rt) {
				close(resume)
				t.Fatal("trusted upsert did not upgrade same runtime")
			}
			close(resume)
			select {
			case e := <-done:
				if path == "claim" && e != nil {
					t.Fatal(e)
				}
				if path == "enqueue" && e == nil {
					t.Fatal("managed delete/maintenance upgrade inserted task without rediscovery")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			row, err := db.New(pool).GetAgentTask(ctx, util.MustParseUUID(task))
			if err != nil || row.Status != "queued" {
				t.Fatalf("upgraded claim changed queue: %s %v", row.Status, err)
			}
		})
	}
}

func ownedFleetNamespace(t *testing.T, pool *pgxpool.Pool, f *testutil.Fixture, prefix string) string {
	t.Helper()
	ns := prefix + uuid.NewString()
	owner := f.UserID
	// Registered before node/runtime children: this assertion runs after their cleanup.
	t.Cleanup(func() {
		var remaining int
		err := pool.QueryRow(context.Background(), `SELECT
 (SELECT count(*) FROM fleet_nodes WHERE namespace=$1 AND owner_id=$2)+
 (SELECT count(*) FROM fleet_node_operations WHERE namespace=$1 AND owner_id=$2)+
 (SELECT count(*) FROM fleet_node_credentials WHERE namespace=$1 AND owner_id=$2)+
 (SELECT count(*) FROM fleet_credential_profiles WHERE namespace=$1 AND owner_id=$2)`, ns, owner).Scan(&remaining)
		if err != nil || remaining != 0 {
			t.Errorf("owned fleet cleanup namespace=%s remaining=%d err=%v", ns, remaining, err)
		} else {
			t.Logf("Task6Cleanup namespace=%s owner=%s workspace=%s rows=0", ns, owner, f.WorkspaceID)
		}
	})
	return ns
}
