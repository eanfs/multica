package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// recordingFleetGuard counts guard calls and records the runtime each check
// was scoped to. It never mutates state, so a claim that proceeds proves the
// guard is a gate and not a claim substitute.
type recordingFleetGuard struct {
	calls      int
	runtimeIDs []pgtype.UUID
	err        error
}

func (g *recordingFleetGuard) CheckClaim(_ context.Context, _ *db.Queries, runtimeID pgtype.UUID) error {
	g.calls++
	g.runtimeIDs = append(g.runtimeIDs, runtimeID)
	return g.err
}

// TestClaimTaskCallsFleetGuard pins the guard into the claim path: every
// successful claim must have passed the check for the runtime that claim is
// scoped to.
func TestClaimTaskCallsFleetGuard(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	agentID := createClaimCapacityFixture(t, ctx, pool)

	guard := &recordingFleetGuard{}
	svc.FleetGuard = guard

	claim, err := svc.ClaimTask(ctx, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim == nil {
		t.Fatal("claim returned nil with an allowing guard")
	}
	if guard.calls != 1 {
		t.Fatalf("guard calls = %d, want 1", guard.calls)
	}

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}
	if len(guard.runtimeIDs) != 1 || util.UUIDToString(guard.runtimeIDs[0]) != runtimeID {
		t.Fatalf("guard runtime = %v, want %s", guard.runtimeIDs, runtimeID)
	}
}

// TestClaimTaskStopsAtFleetGuard is the barrier contract: a rejected guard
// returns no claim and leaves every queued task untouched. Returning an error
// here would fail the daemon's poll instead of simply withholding work.
func TestClaimTaskStopsAtFleetGuard(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	agentID := createClaimCapacityFixture(t, ctx, pool)

	guard := &recordingFleetGuard{err: errors.New("fleet node unavailable")}
	svc.FleetGuard = guard

	claim, err := svc.ClaimTask(ctx, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim != nil {
		t.Fatal("claim must be nil when the fleet guard rejects")
	}
	if guard.calls != 1 {
		t.Fatalf("guard calls = %d, want 1", guard.calls)
	}

	var dispatched, queued int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'dispatched'), count(*) FILTER (WHERE status = 'queued')
		FROM agent_task_queue WHERE agent_id = $1`, agentID).Scan(&dispatched, &queued); err != nil {
		t.Fatalf("count agent tasks: %v", err)
	}
	if dispatched != 0 {
		t.Fatalf("dispatched tasks = %d, want 0", dispatched)
	}
	if queued != 2 {
		t.Fatalf("queued tasks = %d, want 2", queued)
	}
}

// TestClaimTasksForRuntimesStopsAtFleetGuard covers the batch path: its
// per-candidate claims run the same guard, so one rejected guard check must
// withhold every task in the batch instead of only skipping a pre-flight.
func TestClaimTasksForRuntimesStopsAtFleetGuard(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	rt1, rt2 := batchClaimFixture(t, ctx, pool)

	guard := &recordingFleetGuard{err: errors.New("fleet node unavailable")}
	svc.FleetGuard = guard

	claimed, _, err := svc.ClaimTasksForRuntimes(ctx, []pgtype.UUID{util.MustParseUUID(rt1), util.MustParseUUID(rt2)}, 5)
	if err != nil {
		t.Fatalf("claim tasks: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %d tasks, want 0", len(claimed))
	}
	if guard.calls == 0 {
		t.Fatal("batch path never called the fleet guard")
	}

	var dispatched int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue q
		JOIN agent a ON a.id = q.agent_id
		WHERE a.runtime_id IN ($1, $2) AND q.status = 'dispatched'`, rt1, rt2).Scan(&dispatched); err != nil {
		t.Fatalf("count dispatched tasks: %v", err)
	}
	if dispatched != 0 {
		t.Fatalf("dispatched tasks = %d, want 0", dispatched)
	}
}
