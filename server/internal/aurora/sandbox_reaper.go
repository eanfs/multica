package aurora

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/multica-ai/multica/server/internal/util"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/aurorafleet"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// FleetNodeDeleter destroys a workspace's fleet node. It is satisfied by
// *aurorafleet.ControlClient.
type FleetNodeDeleter interface {
	DeleteWorkspaceNode(ctx context.Context, nodeID string) error
}

// SandboxTaskSettler is the single responder that settles failed tasks. It is
// satisfied by *service.TaskService and exists so the reaper never writes a
// settlement path of its own: the transition runs inside the service
// transaction (which also settles delegated failure recoveries) and the
// post-failure side effects stay with HandleFailedTasks.
type SandboxTaskSettler interface {
	FailAuroraSandboxTasksForRuntime(ctx context.Context, arg db.FailAuroraSandboxTasksForRuntimeParams) ([]db.AgentTaskQueue, error)
	HandleFailedTasks(ctx context.Context, tasks []db.AgentTaskQueue) int
}

const (
	// SandboxReapBatchSize bounds the candidate scan in one sweep.
	SandboxReapBatchSize = 100
	// SandboxTaskFailBatchSize bounds task transitions per node per sweep.
	SandboxTaskFailBatchSize = 100

	// sandboxStartingGrace is how long a node may stay starting before the
	// reaper concludes its daemon never arrived.
	sandboxStartingGrace = 2 * time.Minute
	// sandboxIdleTimeout is how long an online node may go without activity
	// before it is stopped.
	sandboxIdleTimeout = 15 * time.Minute
	// sandboxHardLifetime is the maximum wall-clock life of one sandbox node.
	sandboxHardLifetime = 8 * time.Hour
	// sandboxDrainGrace is how long a draining node may keep an active task
	// before that task is failed and the node is stopped.
	sandboxDrainGrace = 30 * time.Minute

	// SandboxRuntimeStartFailed is the failure reason recorded for tasks whose
	// sandbox node never started.
	SandboxRuntimeStartFailed = "runtime_start_failed"
	// SandboxRuntimeLifetimeExceeded is the failure reason recorded for tasks
	// that outlived the node's hard lifetime.
	SandboxRuntimeLifetimeExceeded = "runtime_lifetime_exceeded"
)

// SandboxReaper advances managed sandbox nodes through their lifecycle:
// starting nodes that never enrolled are failed, idle online nodes are stopped,
// nodes past the hard lifetime drain, and draining nodes are stopped once their
// work is finished or the drain grace expires.
type SandboxReaper struct {
	queries *db.Queries
	tx      TxBeginner
	fleet   FleetNodeDeleter
	tasks   SandboxTaskSettler
	now     func() time.Time
}

// NewSandboxReaper wires the reaper to its database, fleet client, task
// settler, and clock. now defaults to time.Now when omitted.
func NewSandboxReaper(queries *db.Queries, tx TxBeginner, fleet FleetNodeDeleter, tasks SandboxTaskSettler, now func() time.Time) *SandboxReaper {
	if now == nil {
		now = time.Now
	}
	return &SandboxReaper{queries: queries, tx: tx, fleet: fleet, tasks: tasks, now: now}
}

// SandboxReapStats reports what one sweep changed.
type SandboxReapStats struct {
	Candidates     int
	StartingFailed int
	Draining       int
	Stopped        int
	Settled        int
}

// Sweep applies one lifecycle pass. It is bounded and safe to repeat: every
// action is idempotent, and only rows this call transitioned are settled.
func (r *SandboxReaper) Sweep(ctx context.Context) (SandboxReapStats, error) {
	now := r.now()
	rows, err := r.queries.ListAuroraSandboxNodesForReap(ctx, db.ListAuroraSandboxNodesForReapParams{
		StartingCutoff: sandboxTimestamp(now.Add(-sandboxStartingGrace)),
		IdleCutoff:     sandboxTimestamp(now.Add(-sandboxIdleTimeout)),
		HardCutoff:     sandboxTimestamp(now.Add(-sandboxHardLifetime)),
		RowLimit:       SandboxReapBatchSize,
	})
	if err != nil {
		return SandboxReapStats{}, fmt.Errorf("list sandbox nodes for reap: %w", err)
	}

	stats := SandboxReapStats{Candidates: len(rows)}
	for _, node := range rows {
		var err error
		switch node.State {
		case "starting":
			err = r.reapStarting(ctx, node, now, &stats)
		case "online":
			err = r.reapOnline(ctx, node, now, &stats)
		case "draining":
			err = r.reapDraining(ctx, node, now, &stats)
		}
		if err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// reapStarting fails a node whose daemon never spent its enrollment, which
// means the sandbox could not start at all.
func (r *SandboxReaper) reapStarting(ctx context.Context, node db.AuroraSandboxNode, now time.Time, stats *SandboxReapStats) error {
	if !node.CreatedAt.Valid || node.CreatedAt.Time.After(now.Add(-sandboxStartingGrace)) {
		return nil
	}
	// A live, unspent enrollment means the daemon may still arrive.
	if sandboxHasLiveEnrollment(node, now) {
		return nil
	}
	if err := r.deleteFleetNode(ctx, node); err != nil {
		return err
	}
	settled, err := r.failTasks(ctx, node, SandboxRuntimeStartFailed)
	if err != nil {
		return err
	}
	stats.Settled += settled
	if _, err := r.queries.FailAuroraSandboxNode(ctx, db.FailAuroraSandboxNodeParams{
		ID:            node.ID,
		FailureReason: pgtype.Text{String: SandboxRuntimeStartFailed, Valid: true},
		WorkspaceID:   node.WorkspaceID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("mark sandbox node failed: %w", err)
	}
	stats.StartingFailed++
	return nil
}

// reapOnline drains a node past its hard lifetime, stops an idle one, and
// leaves a busy one alone.
func (r *SandboxReaper) reapOnline(ctx context.Context, node db.AuroraSandboxNode, now time.Time, stats *SandboxReapStats) error {
	if node.StartedAt.Valid && node.StartedAt.Time.Before(now.Add(-sandboxHardLifetime)) {
		if err := r.markDraining(ctx, node); err != nil {
			return err
		}
		stats.Draining++
		return nil
	}
	if !node.LastActiveAt.Valid || node.LastActiveAt.Time.After(now.Add(-sandboxIdleTimeout)) {
		return nil
	}
	active, err := r.activeTasks(ctx, node)
	if err != nil {
		return err
	}
	if active > 0 {
		return nil
	}
	return r.stopNode(ctx, node, stats)
}

// reapDraining stops a node with no work left and, once the drain grace has
// passed, fails the work that is still holding it.
func (r *SandboxReaper) reapDraining(ctx context.Context, node db.AuroraSandboxNode, now time.Time, stats *SandboxReapStats) error {
	active, err := r.activeTasks(ctx, node)
	if err != nil {
		return err
	}
	if active == 0 {
		return r.stopNode(ctx, node, stats)
	}
	if !node.DrainStartedAt.Valid || node.DrainStartedAt.Time.After(now.Add(-sandboxDrainGrace)) {
		return nil
	}
	settled, err := r.failTasks(ctx, node, SandboxRuntimeLifetimeExceeded)
	if err != nil {
		return err
	}
	stats.Settled += settled
	return r.stopNode(ctx, node, stats)
}

// markDraining starts the drain window for a node that outlived its lifetime.
func (r *SandboxReaper) markDraining(ctx context.Context, node db.AuroraSandboxNode) error {
	if _, err := r.queries.MarkAuroraSandboxNodeDraining(ctx, db.MarkAuroraSandboxNodeDrainingParams{
		WorkspaceID: node.WorkspaceID,
		DaemonID:    node.DaemonID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("mark sandbox node draining: %w", err)
	}
	return nil
}

// stopNode tears the node down: the fleet destroys the containers, then one
// transaction revokes the daemon tokens, takes the runtime offline, and marks
// the node stopped. Fleet not-found is success, so a repeated sweep is a no-op.
func (r *SandboxReaper) stopNode(ctx context.Context, node db.AuroraSandboxNode, stats *SandboxReapStats) error {
	if err := r.deleteFleetNode(ctx, node); err != nil {
		return err
	}
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin sandbox stop transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := r.queries.WithTx(tx)

	daemonID := node.DaemonID
	if daemonID != "" {
		if _, err := qtx.DeleteDaemonTokensByWorkspaceAndDaemons(ctx, db.DeleteDaemonTokensByWorkspaceAndDaemonsParams{
			WorkspaceID: node.WorkspaceID,
			DaemonIds:   []string{daemonID},
		}); err != nil {
			return fmt.Errorf("revoke sandbox daemon tokens: %w", err)
		}
	}
	if err := qtx.ReleaseAuroraManagedRuntime(ctx, db.ReleaseAuroraManagedRuntimeParams{
		ID:          node.RuntimeID,
		WorkspaceID: node.WorkspaceID,
		DaemonID:    pgtype.Text{String: daemonID, Valid: daemonID != ""},
	}); err != nil {
		return fmt.Errorf("release managed runtime: %w", err)
	}
	if _, err := qtx.MarkAuroraSandboxNodeStopped(ctx, db.MarkAuroraSandboxNodeStoppedParams{
		WorkspaceID: node.WorkspaceID,
		DaemonID:    daemonID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("mark sandbox node stopped: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit sandbox stop: %w", err)
	}
	stats.Stopped++
	return nil
}

// deleteFleetNode removes the node from the fleet. A node that never reached
// the fleet, or one the fleet no longer knows, needs no removal.
func (r *SandboxReaper) deleteFleetNode(ctx context.Context, node db.AuroraSandboxNode) error {
	if !node.BackendNodeID.Valid || node.BackendNodeID.String == "" {
		return nil
	}
	// The control API addresses a node by the UUID the server issued at ensure
	// time; the backend resolves it to its own sandbox container through the
	// controlled node label. Sending the fleet's backend name here would fail
	// the route's UUID check and leave the node un-reapable.
	nodeID := util.UUIDToString(node.ID)
	if err := r.fleet.DeleteWorkspaceNode(ctx, nodeID); err != nil {
		if errors.Is(err, aurorafleet.ErrNodeNotFound) {
			return nil
		}
		return fmt.Errorf("delete fleet node %s: %w", nodeID, err)
	}
	return nil
}

// failTasks transitions the node's active tasks and settles exactly the rows
// this call moved, so a repeated sweep cannot refund twice.
func (r *SandboxReaper) failTasks(ctx context.Context, node db.AuroraSandboxNode, reason string) (int, error) {
	failed, err := r.tasks.FailAuroraSandboxTasksForRuntime(ctx, db.FailAuroraSandboxTasksForRuntimeParams{
		RuntimeID:     node.RuntimeID,
		FailureReason: pgtype.Text{String: reason, Valid: true},
		RowLimit:      SandboxTaskFailBatchSize,
	})
	if err != nil {
		return 0, fmt.Errorf("fail sandbox tasks: %w", err)
	}
	if len(failed) == 0 {
		return 0, nil
	}
	return r.tasks.HandleFailedTasks(ctx, failed), nil
}

// activeTasks counts the work still holding the node's runtime.
func (r *SandboxReaper) activeTasks(ctx context.Context, node db.AuroraSandboxNode) (int64, error) {
	count, err := r.queries.CountActiveAuroraSandboxTasks(ctx, node.RuntimeID)
	if err != nil {
		return 0, fmt.Errorf("count active sandbox tasks: %w", err)
	}
	return count, nil
}

// sandboxHasLiveEnrollment reports whether an unconsumed, unexpired secret is
// still waiting for a daemon.
func sandboxHasLiveEnrollment(node db.AuroraSandboxNode, now time.Time) bool {
	if !node.EnrollmentTokenHash.Valid || node.EnrollmentConsumedAt.Valid {
		return false
	}
	return node.EnrollmentExpiresAt.Valid && node.EnrollmentExpiresAt.Time.After(now)
}

// sandboxTimestamp builds a microsecond-precision bound so a local macOS clock
// and the Postgres timestamptz column agree.
func sandboxTimestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC().Truncate(time.Microsecond), Valid: true}
}
