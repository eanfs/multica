package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// newAuroraCompletionService returns a TaskService wired with the credit
// service, plus the fixture ids its Aurora settlement needs. The credit
// service shares the service's pool/query so reserve/refund runs in one
// accounting instance, exactly like the production wiring.
func newAuroraCompletionService(t *testing.T) (svc *TaskService, pool *pgxpool.Pool, workspaceID, userID, agentID string) {
	t.Helper()
	pool = sharedTestPool(t)
	workspaceID, userID, agentID, _ = seedAttributionFixture(t, pool)
	// credit_balance / credit_ledger carry no FK to user, so the fixture's own
	// user cleanup would leave them behind. Remove them here, before the user
	// row is deleted (cleanups run last-registered-first).
	t.Cleanup(func() {
		ctx := context.Background()
		pool.Exec(ctx, `DELETE FROM credit_ledger WHERE user_id = $1`, userID)
		pool.Exec(ctx, `DELETE FROM credit_balance WHERE user_id = $1`, userID)
	})
	q := db.New(pool)
	svc = NewTaskService(q, pool, nil, events.New())
	svc.Credit = aurora.NewCreditService(q, pool)
	return svc, pool, workspaceID, userID, agentID
}

// seedAuroraTask inserts a queued task on agentID and a generation row that
// reverse-links to it via task_id, returning both ids. The rows are removed on
// cleanup (there is no FK, so the fixture's own cleanup would leave them).
func seedAuroraTask(t *testing.T, pool *pgxpool.Pool, workspaceID, userID, agentID string, reserved int64) (generationID, taskID pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	// An active (queued/running) task must carry a runtime_id (check constraint
	// agent_task_queue_active_requires_runtime); reuse the agent's runtime.
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("load agent runtime: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority)
		VALUES ($1, $2, 'queued', 0) RETURNING id`, agentID, runtimeID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	if err := pool.QueryRow(ctx, `
		INSERT INTO aurora_generation (workspace_id, user_id, skill_id, prompt, status, task_id, credits_reserved)
		VALUES ($1, $2, 'xhs-image', 'test prompt', 'queued', $3, $4) RETURNING id`,
		workspaceID, userID, taskID, reserved).Scan(&generationID); err != nil {
		t.Fatalf("seed generation: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM aurora_generation WHERE id = $1`, generationID) })
	return generationID, taskID
}

func auroraGenRow(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID) db.AuroraGeneration {
	t.Helper()
	// Read by id directly: GetAuroraGeneration filters by workspace_id, which the
	// settlement path does not carry separately, so the raw read mirrors what the
	// settlement actually observed.
	var gen db.AuroraGeneration
	if err := pool.QueryRow(context.Background(), `
		SELECT id, workspace_id, user_id, skill_id, prompt, status, task_id,
		       credits_reserved, credits_charged, error, created_at, updated_at
		FROM aurora_generation WHERE id = $1`, id).Scan(
		&gen.ID, &gen.WorkspaceID, &gen.UserID, &gen.SkillID, &gen.Prompt, &gen.Status,
		&gen.TaskID, &gen.CreditsReserved, &gen.CreditsCharged, &gen.Error,
		&gen.CreatedAt, &gen.UpdatedAt); err != nil {
		t.Fatalf("read generation: %v", err)
	}
	return gen
}

func TestAuroraCompletionChargesReserved(t *testing.T) {
	svc, pool, workspaceID, userID, agentID := newAuroraCompletionService(t)
	genID, taskID := seedAuroraTask(t, pool, workspaceID, userID, agentID, 620_000_000)

	svc.settleAuroraOnCompleted(context.Background(), db.AgentTaskQueue{ID: taskID})

	gen := auroraGenRow(t, pool, genID)
	if gen.Status != "completed" {
		t.Fatalf("status = %q, want completed", gen.Status)
	}
	if gen.CreditsCharged != 620_000_000 {
		t.Fatalf("credits_charged = %d, want 620000000", gen.CreditsCharged)
	}
	// Completion charges what was already reserved; the wallet must not move.
	bal, err := svc.Credit.Balance(context.Background(), gen.UserID)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance = %d, want 0 (reservation is the only debit)", bal)
	}
}

func TestAuroraCompletionFailureRefunds(t *testing.T) {
	svc, pool, workspaceID, userID, agentID := newAuroraCompletionService(t)
	ctx := context.Background()
	user := util.MustParseUUID(userID)
	ws := util.MustParseUUID(workspaceID)

	// Fund the wallet so the reservation can be modelled as already spent, then
	// reserve (simulating creation) so the refund has something to return. The
	// reference is namespaced by user id: the idempotency key is globally
	// unique, and a constant would satisfy a later run's fast path against a
	// row left by an abnormally killed run.
	if err := svc.Credit.Grant(ctx, user, ws, 1_000_000_000, aurora.LedgerKindAdjustment, "settle-failed-seed-"+userID); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	genID, taskID := seedAuroraTask(t, pool, workspaceID, userID, agentID, 620_000_000)
	if err := svc.Credit.Reserve(ctx, user, ws, 620_000_000, util.UUIDToString(genID)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	svc.settleAuroraOnFailed(context.Background(), db.AgentTaskQueue{ID: taskID}, "agent_error")

	gen := auroraGenRow(t, pool, genID)
	if gen.Status != "failed" {
		t.Fatalf("status = %q, want failed", gen.Status)
	}
	if !gen.Error.Valid || gen.Error.String != "agent_error" {
		t.Fatalf("error = %q, want agent_error", gen.Error.String)
	}
	if gen.CreditsCharged != 0 {
		t.Fatalf("credits_charged = %d, want 0", gen.CreditsCharged)
	}
	// The reservation is fully returned.
	bal, err := svc.Credit.Balance(ctx, user)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 1_000_000_000 {
		t.Fatalf("balance after refund = %d, want 1000000000", bal)
	}

	// Idempotency: a second settlement of the same terminal event is a no-op.
	svc.settleAuroraOnFailed(context.Background(), db.AgentTaskQueue{ID: taskID}, "agent_error")
	bal, err = svc.Credit.Balance(ctx, user)
	if err != nil {
		t.Fatalf("Balance (retry): %v", err)
	}
	if bal != 1_000_000_000 {
		t.Fatalf("balance after idempotent retry = %d, want 1000000000", bal)
	}
}

func TestAuroraCompletionNoOpForNonAuroraTask(t *testing.T) {
	svc, pool, _, _, agentID := newAuroraCompletionService(t)
	ctx := context.Background()

	// A task with no generation must settle to a silent no-op.
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("load agent runtime: %v", err)
	}
	var taskID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority)
		VALUES ($1, $2, 'queued', 0) RETURNING id`, agentID, runtimeID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	svc.settleAuroraOnCompleted(ctx, db.AgentTaskQueue{ID: taskID})
	svc.settleAuroraOnFailed(ctx, db.AgentTaskQueue{ID: taskID}, "agent_error")

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM aurora_generation WHERE task_id = $1`, taskID).Scan(&n); err != nil {
		t.Fatalf("count generations: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no generation for a non-Aurora task, found %d", n)
	}
}
