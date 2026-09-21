package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Aurora settlement runs when an agent task that backs an Aurora generation
// reaches a terminal state. A generation is looked up by task_id (the reverse
// link written at enqueue), and only a non-terminal generation is settled:
// replaying the same terminal event is a no-op, which is what makes the
// daemon's durable complete/fail callbacks and the sweeper's failure passes
// safe to re-run.
//
// Success and failure are intentionally asymmetric:
//
//   - Success charges the amount already reserved at creation. No balance
//     moves here — the reservation deducted it — settling only records
//     credits_charged = credits_reserved and flips the status to completed.
//   - Failure refunds the reservation (idempotent via the generation-id
//     reference) and records the reason. The refund runs before the status
//     flip so a failed refund leaves the generation non-terminal and a retry
//     re-attempts it instead of being short-circuited by a terminal status.
const (
	auroraStatusCompleted = "completed"
	auroraStatusFailed    = "failed"
)

// auroraTerminal reports whether a generation status is terminal.
func auroraTerminal(status string) bool {
	return status == auroraStatusCompleted || status == auroraStatusFailed
}

// settleAuroraOnCompleted settles a generation whose backing task completed.
// It is a best-effort, idempotent post-commit side effect: the credits were
// already deducted at reservation, so a failure here leaves the wallet correct
// and only the row status stale (re-armed by a daemon replay).
func (s *TaskService) settleAuroraOnCompleted(ctx context.Context, task db.AgentTaskQueue) {
	gen, err := s.Queries.GetAuroraGenerationByTaskID(ctx, task.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return // not an Aurora task
		}
		slog.Warn("aurora completion: load generation failed",
			"task_id", util.UUIDToString(task.ID), "error", err)
		return
	}
	if auroraTerminal(gen.Status) {
		return
	}
	if _, err := s.Queries.UpdateAuroraGenerationTerminal(ctx, db.UpdateAuroraGenerationTerminalParams{
		ID:             gen.ID,
		Status:         auroraStatusCompleted,
		Error:          pgtype.Text{},
		CreditsCharged: gen.CreditsReserved,
		WorkspaceID:    gen.WorkspaceID,
	}); err != nil {
		slog.Warn("aurora completion: mark completed failed",
			"generation_id", util.UUIDToString(gen.ID), "error", err)
	}
}

// settleAuroraOnFailed settles a generation whose backing task failed (or was
// swept into a terminal failed state). It refunds the reserved credits and
// records the reason. The refund is attempted first so a failed refund leaves
// the generation non-terminal for a later retry.
func (s *TaskService) settleAuroraOnFailed(ctx context.Context, task db.AgentTaskQueue, reason string) {
	gen, err := s.Queries.GetAuroraGenerationByTaskID(ctx, task.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return // not an Aurora task
		}
		slog.Warn("aurora completion: load generation failed",
			"task_id", util.UUIDToString(task.ID), "error", err)
		return
	}
	if auroraTerminal(gen.Status) {
		return
	}
	if reason == "" {
		reason = auroraStatusFailed
	}
	if s.Credit != nil && gen.CreditsReserved > 0 {
		if err := s.Credit.Refund(ctx, gen.UserID, gen.WorkspaceID, gen.CreditsReserved, util.UUIDToString(gen.ID)); err != nil {
			slog.Warn("aurora completion: refund failed",
				"generation_id", util.UUIDToString(gen.ID), "error", err)
			return
		}
	}
	if _, err := s.Queries.UpdateAuroraGenerationTerminal(ctx, db.UpdateAuroraGenerationTerminalParams{
		ID:             gen.ID,
		Status:         auroraStatusFailed,
		Error:          pgtype.Text{String: reason, Valid: true},
		CreditsCharged: 0,
		WorkspaceID:    gen.WorkspaceID,
	}); err != nil {
		slog.Warn("aurora completion: mark failed failed",
			"generation_id", util.UUIDToString(gen.ID), "error", err)
	}
}
