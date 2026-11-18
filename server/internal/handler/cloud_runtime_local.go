package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/cloudruntime"
	"github.com/multica-ai/multica/server/internal/fleet/barrier"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Local Fleet lifecycle states mirrored from the Fleet package so the API can
// coordinate operations without importing the Docker-backed Fleet service.
const (
	localFleetStateLaunching       = "launching"
	localFleetStateStarting        = "starting"
	localFleetStateRunning         = "running"
	localFleetStateStopping        = "stopping"
	localFleetStateStopped         = "stopped"
	localFleetStateRebooting       = "rebooting"
	localFleetStateTerminating     = "terminating"
	localFleetStateTerminated      = "terminated"
	localFleetStateFailed          = "failed"
	localFleetStateInstanceMissing = "instance_missing"

	localFleetOpPrepared = "prepared"
	localFleetOpAborted  = "aborted"
	localFleetOpFailed   = "failed"

	localFleetHealthFreshWindow = 30 * time.Second
	localFleetDiagnoseTimeout   = 5 * time.Second
	localFleetDiagnosePath      = "/api/v1/nodes/diagnose"
)

// Errors returned by Prepare. The HTTP proxy maps each into a stable status:
// 404 for a missing/foreign node, 409 for busy or conflicting requests.
var (
	ErrNodeBusy          = errors.New("node busy")
	ErrOperationConflict = errors.New("operation conflict")
	ErrNodeNotFound      = errors.New("node not found")
)

var (
	errLocalFleetRefRequired         = errors.New("exactly one of instance_id or node_id is required")
	errLocalFleetInvalidOperation    = errors.New("invalid local fleet operation")
	errLocalFleetInvalidNodeID       = errors.New("invalid fleet node id")
	errLocalFleetIdempotencyMissing  = errors.New("idempotency key is required")
	errLocalFleetDiagnoseUnavailable = errors.New("node diagnosis unavailable")
	// errLocalFleetStateUncertain mirrors Fleet's ErrStateUncertain: the node
	// may still be running, but its diagnostic surface cannot be read. That is
	// different from a container that is known to be stopped or missing.
	errLocalFleetStateUncertain = errors.New("node state uncertain")
)

// localFleetNodeRef addresses one node by Docker container id or node UUID.
// Exactly one of the two fields may be set.
type localFleetNodeRef struct {
	InstanceID string
	NodeID     string
}

// localFleetCoordinator serializes maintenance requests for local Fleet nodes
// against the shared PostgreSQL database. It only prepares and approves
// operations; the Fleet coordinator (a separate process) executes approved
// rows with the Docker provider.
type localFleetCoordinator struct {
	pool      *pgxpool.Pool
	namespace string
	// diagnose reads the node daemon's pending terminal reports through the
	// Fleet diagnostic endpoint. It runs outside every database transaction so
	// a slow Docker exec never holds a node lock.
	diagnose func(ctx context.Context, ownerID, containerID string) (int, error)
	now      func() time.Time
}

func newLocalFleetCoordinator(pool *pgxpool.Pool, namespace string, diagnose func(context.Context, string, string) (int, error), now func() time.Time) *localFleetCoordinator {
	if now == nil {
		now = time.Now
	}
	return &localFleetCoordinator{
		pool:      pool,
		namespace: strings.TrimSpace(namespace),
		diagnose:  diagnose,
		now:       now,
	}
}

func localFleetTargetState(operation string) (string, bool) {
	switch operation {
	case "stop":
		return localFleetStateStopping, true
	case "reboot":
		return localFleetStateRebooting, true
	case "delete":
		return localFleetStateTerminating, true
	default:
		return "", false
	}
}

// Prepare schedules one stop/reboot/delete operation.
//
// It runs in two short transactions separated by the Fleet diagnosis:
//
//  1. under the node's exclusive advisory lock, resolve the node, replay an
//     identical idempotency key, reject busy nodes, and persist the prepared
//     operation together with the transitional target state;
//  2. outside any transaction, read the daemon's pending terminal reports;
//  3. under the same lock, verify the prepared operation and generation, then
//     either abort the barrier (busy) or atomically approve the operation.
//
// The lock is the same barrier.Key namespace Fleet claims use, so a prepared
// operation blocks new work before the diagnosis starts.
func (c *localFleetCoordinator) Prepare(ctx context.Context, ownerID string, ref localFleetNodeRef, operation, idempotencyKey string) (db.FleetNode, error) {
	if c == nil || c.pool == nil {
		return db.FleetNode{}, errors.New("local fleet coordinator is not configured")
	}
	targetState, ok := localFleetTargetState(operation)
	if !ok {
		return db.FleetNode{}, fmt.Errorf("%w: %q", errLocalFleetInvalidOperation, operation)
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return db.FleetNode{}, errLocalFleetIdempotencyMissing
	}
	ownerUUID, err := util.ParseUUID(ownerID)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("owner id: %w", err)
	}

	node, priorTarget, op, replayed, err := c.prepare(ctx, ownerUUID, ref, operation, idempotencyKey, targetState)
	if err != nil {
		return db.FleetNode{}, err
	}
	if replayed {
		return node, nil
	}

	// A container that is known to be stopped, gone, or settled cannot have
	// pending terminal reports: there is no running daemon to drain them.
	// Diagnosing it would only return state_uncertain, which must never keep a
	// delete prepared forever. Only a container that may still be running needs
	// the diagnosis.
	pending := 0
	if localFleetDiagnosisNeeded(node, priorTarget, operation) {
		if c.diagnose == nil {
			return db.FleetNode{}, c.abortPrepare(ctx, node, priorTarget, op, errLocalFleetDiagnoseUnavailable)
		}
		pending, err = c.diagnose(ctx, ownerID, node.ContainerID)
		if err != nil {
			if errors.Is(err, errLocalFleetStateUncertain) {
				// The container may still be running while its diagnostic
				// surface is unreadable. Refuse the operation and keep the
				// barrier: an unreadable running daemon might still hold work.
				c.recordDiagnoseFailure(ctx, op)
				return db.FleetNode{}, fmt.Errorf("%w: %w", errLocalFleetDiagnoseUnavailable, err)
			}
			// Any other diagnosis failure is a transport or protocol problem,
			// not evidence that work is pending. Release the barrier instead of
			// leaving a prepared operation that can never be approved.
			return db.FleetNode{}, c.abortPrepare(ctx, node, priorTarget, op,
				fmt.Errorf("%w: %v", errLocalFleetDiagnoseUnavailable, err))
		}
	}
	return c.finishPrepare(ctx, node, priorTarget, op, pending)
}

// prepare is the first short transaction. It returns the pre-update target so
// an abort can restore the state the node had before this request, and
// replayed=true when the idempotency key already described this exact request
// and its operation is final.
func (c *localFleetCoordinator) prepare(ctx context.Context, ownerID pgtype.UUID, ref localFleetNodeRef, operation, idempotencyKey, targetState string) (db.FleetNode, string, db.FleetNodeOperation, bool, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("begin prepare: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)

	node, err := c.loadNode(ctx, q, ownerID, ref)
	if err != nil {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, err
	}
	if err := barrier.AcquireExclusive(ctx, tx, barrier.Key(c.namespace, node.DaemonID)); err != nil {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, err
	}
	// Reload under the lock: the state checked below must be the state every
	// other coordinator sees while holding the same key.
	node, err = q.GetFleetNode(ctx, db.GetFleetNodeParams{ID: node.ID, Namespace: c.namespace})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FleetNode{}, "", db.FleetNodeOperation{}, false, ErrNodeNotFound
		}
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("reload node: %w", err)
	}

	fingerprint := localFleetRequestFingerprint(operation, node)
	existing, err := q.GetFleetNodeOperationByIdempotencyKey(ctx, idempotencyKey)
	switch {
	case err == nil:
		if existing.NodeID != node.ID || existing.RequestFingerprint != fingerprint {
			return db.FleetNode{}, "", db.FleetNodeOperation{}, false, ErrOperationConflict
		}
		switch existing.Phase {
		case localFleetOpPrepared:
			// A previous attempt prepared the barrier but never finished (for
			// example state was uncertain). Re-run the outside-lock diagnosis
			// and approval for this same operation instead of reporting an
			// accepted operation that can never execute.
			if err := tx.Commit(ctx); err != nil {
				return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("commit prepared replay: %w", err)
			}
			return node, localFleetReplayPriorTarget(node), existing, false, nil
		case localFleetOpAborted:
			// The earlier attempt was rejected after diagnosis. Replaying it
			// must keep surfacing the same busy result instead of pretending a
			// maintenance operation is now approved.
			return db.FleetNode{}, "", db.FleetNodeOperation{}, false, ErrNodeBusy
		case localFleetOpFailed:
			return db.FleetNode{}, "", db.FleetNodeOperation{}, false, ErrOperationConflict
		default:
			// approved, executing, and done are all in flight or already
			// applied for this same fingerprint. Replaying the request reports
			// the node instead of treating the retry as a second operation.
			if err := tx.Commit(ctx); err != nil {
				return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("commit idempotent prepare: %w", err)
			}
			return node, "", existing, true, nil
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("load idempotent operation: %w", err)
	}

	// Another maintenance operation still awaiting execution would race this
	// one for the same node. Refuse it instead of overwriting the prepared
	// generation that the first operation's approval check compares against.
	inflight, err := q.CountActiveFleetNodeOperationsForNode(ctx, node.ID)
	if err != nil {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("count active operations: %w", err)
	}
	if inflight > 0 {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, ErrOperationConflict
	}

	// Validate the node as it was before this transaction changes its target.
	// Doing it in the approval transaction would only ever see the transitional
	// state this request just wrote, which made the terminal-state checks dead.
	if err := localFleetValidateTarget(node, operation); err != nil {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, err
	}
	if operation != "delete" && node.ContainerID == "" {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false,
			fmt.Errorf("%w: node has no container", ErrOperationConflict)
	}

	// Busy checks cover every workspace whose runtime belongs to this owner;
	// the node itself is owner-scoped, not workspace-scoped.
	if err := c.checkDaemonBusy(ctx, q, node, operation); err != nil {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, err
	}

	priorTarget := node.TargetState
	if err := q.UpdateFleetNodeTargetState(ctx, db.UpdateFleetNodeTargetStateParams{
		ID:          node.ID,
		Namespace:   c.namespace,
		TargetState: targetState,
	}); err != nil {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("record maintenance target: %w", err)
	}
	node, err = q.GetFleetNode(ctx, db.GetFleetNodeParams{ID: node.ID, Namespace: c.namespace})
	if err != nil {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("reload prepared node: %w", err)
	}

	op, err := q.CreateFleetNodeOperation(ctx, db.CreateFleetNodeOperationParams{
		NodeID:             node.ID,
		Operation:          operation,
		Phase:              localFleetOpPrepared,
		IdempotencyKey:     idempotencyKey,
		RequestFingerprint: fingerprint,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// A concurrent request with the same key won the insert. Roll this
			// attempt back and replay the row that won, so two identical
			// requests do not turn into a 409 for the loser.
			_ = tx.Rollback(ctx)
			return c.replayByIdempotencyKey(ctx, node.ID, fingerprint, idempotencyKey)
		}
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("create operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("commit prepare: %w", err)
	}
	return node, priorTarget, op, false, nil
}

// finishPrepare is the second short transaction. It verifies that the prepared
// operation still owns the node, rechecks busy state with the same owner +
// daemon scope used by prepare, and then approves or aborts atomically.
func (c *localFleetCoordinator) finishPrepare(ctx context.Context, prepared db.FleetNode, priorTarget string, op db.FleetNodeOperation, pending int) (db.FleetNode, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("begin approve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)

	if err := barrier.AcquireExclusive(ctx, tx, barrier.Key(c.namespace, prepared.DaemonID)); err != nil {
		return db.FleetNode{}, err
	}
	node, err := q.GetFleetNode(ctx, db.GetFleetNodeParams{ID: prepared.ID, Namespace: c.namespace})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FleetNode{}, ErrNodeNotFound
		}
		return db.FleetNode{}, fmt.Errorf("reload node for approval: %w", err)
	}
	current, err := q.GetFleetNodeOperation(ctx, op.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FleetNode{}, ErrOperationConflict
		}
		return db.FleetNode{}, fmt.Errorf("reload operation: %w", err)
	}
	switch current.Phase {
	case localFleetOpPrepared:
		// Still this request's barrier; verify and decide below.
	case "approved", "executing", "done":
		// A concurrent retry of the same request already approved the row.
		// Report the node instead of failing the retry with a conflict.
		if err := tx.Commit(ctx); err != nil {
			return db.FleetNode{}, fmt.Errorf("commit idempotent replay: %w", err)
		}
		return node, nil
	case localFleetOpAborted:
		return db.FleetNode{}, ErrNodeBusy
	case localFleetOpFailed:
		return db.FleetNode{}, ErrOperationConflict
	default:
		return db.FleetNode{}, ErrOperationConflict
	}

	abort := func(cause error) (db.FleetNode, error) {
		if err := c.abortPreparedTx(ctx, q, prepared, priorTarget, op.ID); err != nil {
			return db.FleetNode{}, fmt.Errorf("%w: abort prepared operation: %v", cause, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return db.FleetNode{}, fmt.Errorf("%w: commit abort: %v", cause, err)
		}
		return db.FleetNode{}, cause
	}

	// The approval must compare against the generation the prepare transaction
	// produced. A change means another coordinator settled the node first.
	if node.Generation != prepared.Generation {
		return abort(fmt.Errorf("%w: node generation changed after prepare", ErrOperationConflict))
	}
	if node.TargetState == localFleetStateTerminated || node.TargetState == localFleetStateFailed {
		return abort(fmt.Errorf("%w: node settled as %s", ErrOperationConflict, node.TargetState))
	}
	// Recheck busy here because prepare released its lock before the diagnosis.
	// A task dispatched in that window must invalidate the operation.
	if err := c.checkDaemonBusy(ctx, q, node, op.Operation); err != nil {
		return abort(err)
	}
	if pending > 0 {
		return abort(fmt.Errorf("%w: node has pending terminal reports", ErrNodeBusy))
	}

	affected, err := q.ApproveFleetNodeOperation(ctx, op.ID)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("approve operation: %w", err)
	}
	if affected != 1 {
		return abort(fmt.Errorf("%w: operation phase changed before approval", ErrOperationConflict))
	}
	if err := tx.Commit(ctx); err != nil {
		return db.FleetNode{}, fmt.Errorf("commit approve: %w", err)
	}
	return node, nil
}

// abortPrepare releases a prepared operation from outside a transaction when
// the diagnosis failed for a reason that is not state uncertainty.
func (c *localFleetCoordinator) abortPrepare(ctx context.Context, prepared db.FleetNode, priorTarget string, op db.FleetNodeOperation, cause error) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return cause
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)
	if err := barrier.AcquireExclusive(ctx, tx, barrier.Key(c.namespace, prepared.DaemonID)); err != nil {
		return cause
	}
	if err := c.abortPreparedTx(ctx, q, prepared, priorTarget, op.ID); err != nil {
		return fmt.Errorf("%w: %v", cause, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit abort: %v", cause, err)
	}
	return cause
}

// abortPreparedTx moves a still-prepared operation to aborted and restores the
// target captured before prepare, but only while this operation still owns the
// node's transitional target. A target another actor already changed is left
// alone rather than clobbered.
func (c *localFleetCoordinator) abortPreparedTx(ctx context.Context, q *db.Queries, prepared db.FleetNode, priorTarget string, opID pgtype.UUID) error {
	if _, err := q.RecordFleetNodeOperationPrepareFailure(ctx, db.RecordFleetNodeOperationPrepareFailureParams{
		ID:    opID,
		Error: "operation aborted before approval",
	}); err != nil {
		return fmt.Errorf("record abort reason: %w", err)
	}
	affected, err := q.MarkFleetNodeOperationPhase(ctx, db.MarkFleetNodeOperationPhaseParams{
		ID:            opID,
		Phase:         localFleetOpAborted,
		ExpectedPhase: localFleetOpPrepared,
	})
	if err != nil {
		return fmt.Errorf("abort operation: %w", err)
	}
	if affected != 1 {
		// Another request already moved this row; its path owns the target.
		return nil
	}
	current, err := q.GetFleetNode(ctx, db.GetFleetNodeParams{ID: prepared.ID, Namespace: c.namespace})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("reload node for abort: %w", err)
	}
	if current.TargetState != prepared.TargetState || current.TargetState == priorTarget {
		return nil
	}
	if err := q.UpdateFleetNodeTargetState(ctx, db.UpdateFleetNodeTargetStateParams{
		ID:          current.ID,
		Namespace:   c.namespace,
		TargetState: priorTarget,
	}); err != nil {
		return fmt.Errorf("restore node target state: %w", err)
	}
	return nil
}

// checkDaemonBusy applies the design's two busy levels for the owner + daemon
// pair: stop/reboot reject active work, delete also rejects queued work. It is
// used by both prepare and the approval recheck.
func (c *localFleetCoordinator) checkDaemonBusy(ctx context.Context, q *db.Queries, node db.FleetNode, operation string) error {
	active, err := q.CountActiveTasksForFleetDaemon(ctx, db.CountActiveTasksForFleetDaemonParams{
		DaemonID: util.StrToText(node.DaemonID),
		OwnerID:  node.OwnerID,
	})
	if err != nil {
		return fmt.Errorf("count active tasks: %w", err)
	}
	if active > 0 {
		return fmt.Errorf("%w: daemon has active tasks", ErrNodeBusy)
	}
	if operation != "delete" {
		return nil
	}
	queued, err := q.CountQueuedTasksForFleetDaemon(ctx, db.CountQueuedTasksForFleetDaemonParams{
		DaemonID: util.StrToText(node.DaemonID),
		OwnerID:  node.OwnerID,
	})
	if err != nil {
		return fmt.Errorf("count queued tasks: %w", err)
	}
	if queued > 0 {
		return fmt.Errorf("%w: daemon has queued tasks", ErrNodeBusy)
	}
	return nil
}

// localFleetValidateTarget rejects operations the node's pre-operation target
// cannot accept. The target must be the one loaded before prepare rewrites it.
func localFleetValidateTarget(node db.FleetNode, operation string) error {
	switch node.TargetState {
	case localFleetStateTerminated:
		return fmt.Errorf("%w: node is terminated", ErrOperationConflict)
	case localFleetStateFailed:
		if operation != "delete" {
			return fmt.Errorf("%w: failed node only accepts delete", ErrOperationConflict)
		}
	case localFleetStateLaunching:
		// A launching node may still be creating its container and volumes, so
		// every maintenance operation waits until the launch settles.
		return fmt.Errorf("%w: node is still launching", ErrNodeBusy)
	}
	return nil
}

// localFleetDiagnosisNeeded reports whether the node may still be running and
// therefore hold pending terminal reports. It uses the target the node had
// before this request: prepare has already written this operation's
// transitional target, and that transitional value says nothing about whether a
// daemon is draining.
//
// A settled or already stopped node never needs the diagnosis. In particular a
// delete issued after a completed stop must not diagnose the stopped container:
// Fleet would answer state_uncertain, which keeps the prepared operation from
// ever being retried into an approval. prepare already rejected any other
// active operation on this node, so a stopping target captured before this
// request can only be the settled result of an earlier stop; the stop request
// itself is the only operation that still needs the pending-report check.
func localFleetDiagnosisNeeded(node db.FleetNode, priorTarget, operation string) bool {
	if node.ContainerID == "" {
		return false
	}
	switch node.ObservedState {
	case localFleetStateStopped, localFleetStateInstanceMissing, localFleetStateTerminated:
		return false
	}
	switch priorTarget {
	case localFleetStateStopped, localFleetStateTerminated:
		return false
	case localFleetStateStopping:
		return operation != "delete"
	default:
		return true
	}
}

// localFleetReplayPriorTarget recovers a settled target for a prepared replay.
// A resumed attempt does not carry the exact target captured by the original
// transaction, so derive one from the observation instead of assuming running.
func localFleetReplayPriorTarget(node db.FleetNode) string {
	switch node.ObservedState {
	case localFleetStateStopped:
		return localFleetStateStopped
	case localFleetStateTerminated:
		return localFleetStateTerminated
	case localFleetStateInstanceMissing, localFleetStateFailed:
		return localFleetStateFailed
	default:
		return localFleetStateRunning
	}
}

// replayByIdempotencyKey reads the operation a concurrent request inserted for
// the same key. A matching fingerprint replays that operation; anything else is
// a genuine conflict.
func (c *localFleetCoordinator) replayByIdempotencyKey(ctx context.Context, nodeID pgtype.UUID, fingerprint, idempotencyKey string) (db.FleetNode, string, db.FleetNodeOperation, bool, error) {
	q := db.New(c.pool)
	existing, err := q.GetFleetNodeOperationByIdempotencyKey(ctx, idempotencyKey)
	if err != nil {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, ErrOperationConflict
	}
	if existing.NodeID != nodeID || existing.RequestFingerprint != fingerprint {
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, ErrOperationConflict
	}
	node, err := q.GetFleetNode(ctx, db.GetFleetNodeParams{ID: nodeID, Namespace: c.namespace})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FleetNode{}, "", db.FleetNodeOperation{}, false, ErrNodeNotFound
		}
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, fmt.Errorf("reload node: %w", err)
	}
	switch existing.Phase {
	case localFleetOpPrepared:
		return node, localFleetReplayPriorTarget(node), existing, false, nil
	case localFleetOpAborted:
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, ErrNodeBusy
	case localFleetOpFailed:
		return db.FleetNode{}, "", db.FleetNodeOperation{}, false, ErrOperationConflict
	default:
		return node, "", existing, true, nil
	}
}

func (c *localFleetCoordinator) loadNode(ctx context.Context, q *db.Queries, ownerID pgtype.UUID, ref localFleetNodeRef) (db.FleetNode, error) {
	instanceID := strings.TrimSpace(ref.InstanceID)
	nodeID := strings.TrimSpace(ref.NodeID)
	if (instanceID == "") == (nodeID == "") {
		return db.FleetNode{}, errLocalFleetRefRequired
	}
	if nodeID != "" {
		id, err := util.ParseUUID(nodeID)
		if err != nil {
			return db.FleetNode{}, errLocalFleetInvalidNodeID
		}
		node, err := q.GetFleetNodeByIDAndOwner(ctx, db.GetFleetNodeByIDAndOwnerParams{
			ID:        id,
			Namespace: c.namespace,
			OwnerID:   ownerID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return db.FleetNode{}, ErrNodeNotFound
			}
			return db.FleetNode{}, fmt.Errorf("load node: %w", err)
		}
		return node, nil
	}

	node, err := q.GetFleetNodeByContainerID(ctx, db.GetFleetNodeByContainerIDParams{
		ContainerID: instanceID,
		Namespace:   c.namespace,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FleetNode{}, ErrNodeNotFound
		}
		return db.FleetNode{}, fmt.Errorf("load node: %w", err)
	}
	if node.OwnerID != ownerID {
		return db.FleetNode{}, ErrNodeNotFound
	}
	return node, nil
}

func (c *localFleetCoordinator) recordDiagnoseFailure(ctx context.Context, op db.FleetNodeOperation) {
	// Only attach the message while the row is still prepared: a concurrent
	// retry may already have approved or aborted it, and writing the phase back
	// would expand the maintenance barrier by accident.
	if _, err := db.New(c.pool).RecordFleetNodeOperationPrepareFailure(ctx, db.RecordFleetNodeOperationPrepareFailureParams{
		ID:    op.ID,
		Error: "diagnosis unavailable",
	}); err != nil {
		slog.Warn("local fleet: record diagnosis failure", "operation", uuidToString(op.ID), "error", err)
	}
}

func localFleetRequestFingerprint(operation string, node db.FleetNode) string {
	subject := node.ContainerID
	if subject == "" {
		subject = uuidToString(node.ID)
	}
	return operation + ":" + subject
}

// localFleetProxyOperation reports whether a proxied request is one this
// server coordinates locally. start/status/diagnose continue to pass through.
func localFleetProxyOperation(method, path string) (string, bool) {
	switch {
	case path == "/api/v1/nodes/stop" && method == http.MethodPost:
		return "stop", true
	case path == "/api/v1/nodes/reboot" && method == http.MethodPost:
		return "reboot", true
	case path == "/api/v1/nodes" && method == http.MethodDelete:
		return "delete", true
	default:
		return "", false
	}
}

func (h *Handler) coordinateLocalFleet(w http.ResponseWriter, r *http.Request, operation string) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}
	body, ok := readCloudRuntimeJSONBody(w, r)
	if !ok {
		return
	}
	var request struct {
		InstanceID string `json:"instance_id"`
		NodeID     string `json:"node_id"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	node, err := h.LocalFleet.Prepare(r.Context(), userID, localFleetNodeRef{
		InstanceID: request.InstanceID,
		NodeID:     request.NodeID,
	}, operation, idempotencyKey)
	if err != nil {
		writeLocalFleetError(w, r, err)
		return
	}
	now := time.Now
	if h.LocalFleet.now != nil {
		now = h.LocalFleet.now
	}
	// The operation is optional: a response is still useful when the journal
	// row cannot be read, but the client should not lose the reason.
	activeOp, opErr := h.LocalFleet.activeOperation(r.Context(), node.ID)
	if opErr != nil {
		slog.Warn("local fleet: read active operation failed",
			append(logger.RequestAttrs(r), "node", uuidToString(node.ID), "error", opErr)...)
	}
	writeJSON(w, http.StatusOK, localFleetNodeJSON(node, now(), activeOp))
}

// activeOperation returns the node's in-flight lifecycle operation, or nil
// when the node is settled. It runs after Prepare committed, so an approved
// row is already visible here.
func (c *localFleetCoordinator) activeOperation(ctx context.Context, nodeID pgtype.UUID) (*db.FleetNodeOperation, error) {
	if c == nil || c.pool == nil {
		return nil, nil
	}
	op, err := db.New(c.pool).GetActiveFleetNodeOperationForNode(ctx, nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &op, nil
}

func writeLocalFleetError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNodeNotFound):
		writeErrorCode(w, http.StatusNotFound, "not_found", "node not found")
	case errors.Is(err, ErrNodeBusy):
		writeErrorCode(w, http.StatusConflict, "node_busy", "node busy")
	case errors.Is(err, ErrOperationConflict):
		writeErrorCode(w, http.StatusConflict, "conflict", "operation conflict")
	case errors.Is(err, errLocalFleetRefRequired),
		errors.Is(err, errLocalFleetInvalidOperation),
		errors.Is(err, errLocalFleetInvalidNodeID),
		errors.Is(err, errLocalFleetIdempotencyMissing):
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "invalid request")
	case errors.Is(err, errLocalFleetStateUncertain):
		writeErrorCode(w, http.StatusServiceUnavailable, "state_uncertain", "node state uncertain")
	default:
		slog.Warn("local fleet operation failed", append(logger.RequestAttrs(r), "error", err)...)
		writeErrorCode(w, http.StatusServiceUnavailable, "local_fleet_unavailable", "local fleet operation failed")
	}
}

// localFleetDiagnose asks Fleet to read the node daemon's health and pending
// terminal reports. Fleet requires the owner identity, so it travels as the
// service-authenticated proxy's X-User-ID.
func (h *Handler) localFleetDiagnose(ctx context.Context, ownerID, containerID string) (int, error) {
	if h.CloudRuntime == nil {
		return 0, errors.New("cloud runtime client is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, localFleetDiagnoseTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"instance_id": containerID})
	if err != nil {
		return 0, err
	}
	resp, err := h.CloudRuntime.Do(ctx, cloudruntime.Request{
		Method: http.MethodPost,
		Path:   localFleetDiagnosePath,
		Body:   body,
		UserID: ownerID,
		Op:     "diagnose",
	})
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		// Fleet reports an unreadable but possibly running daemon as
		// state_uncertain. That must stay distinguishable from a transport
		// failure so the coordinator keeps the barrier only in that case.
		var failure struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(resp.Body, &failure) == nil && failure.Code == "state_uncertain" {
			return 0, fmt.Errorf("%w: fleet diagnose returned state_uncertain", errLocalFleetStateUncertain)
		}
		return 0, fmt.Errorf("fleet diagnose returned status %d", resp.StatusCode)
	}
	var report struct {
		PendingTerminalReportCount int `json:"pending_terminal_report_count"`
	}
	if err := json.Unmarshal(resp.Body, &report); err != nil {
		return 0, fmt.Errorf("decode fleet diagnose: %w", err)
	}
	if report.PendingTerminalReportCount < 0 {
		return 0, errors.New("invalid pending terminal report count")
	}
	return report.PendingTerminalReportCount, nil
}

// localFleetNodeOperation mirrors Fleet's in-flight operation summary.
type localFleetNodeOperation struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
}

// localFleetNodeResponse mirrors Fleet's stable client node contract so local
// mode returns the same JSON shape as the proxied SaaS response.
type localFleetNodeResponse struct {
	ID           string                   `json:"id"`
	OwnerID      string                   `json:"owner_id"`
	InstanceID   string                   `json:"instance_id"`
	Region       string                   `json:"region"`
	InstanceType string                   `json:"instance_type"`
	ImageID      string                   `json:"image_id"`
	SubnetID     string                   `json:"subnet_id"`
	Name         string                   `json:"name"`
	Status       string                   `json:"status"`
	DaemonID     string                   `json:"daemon_id"`
	StatusDetail json.RawMessage          `json:"status_detail,omitempty"`
	Operation    *localFleetNodeOperation `json:"operation,omitempty"`
	Tags         map[string]string        `json:"tags"`
	Metadata     map[string]any           `json:"metadata"`
	CreatedAt    time.Time                `json:"created_at"`
	UpdatedAt    time.Time                `json:"updated_at"`
	Provider     string                   `json:"provider,omitempty"`
	Error        string                   `json:"error,omitempty"`
}

func localFleetNodeJSON(node db.FleetNode, now time.Time, operation *db.FleetNodeOperation) localFleetNodeResponse {
	response := localFleetNodeResponse{
		ID:           uuidToString(node.ID),
		OwnerID:      uuidToString(node.OwnerID),
		InstanceID:   node.ContainerID,
		Region:       "local",
		InstanceType: localFleetNodeSpecID(node.Spec),
		ImageID:      node.Image,
		SubnetID:     "",
		Name:         node.Name,
		Status:       localFleetDisplayStatus(node, now),
		DaemonID:     node.DaemonID,
		Tags:         map[string]string{},
		Metadata: map[string]any{
			"provider":       "docker",
			"daemon_id":      node.DaemonID,
			"target_state":   node.TargetState,
			"observed_state": node.ObservedState,
			"daemon_ready":   node.DaemonReady,
			"generation":     node.Generation,
		},
		CreatedAt: localFleetTimestamp(node.CreatedAt),
		UpdatedAt: localFleetTimestamp(node.UpdatedAt),
		Provider:  "docker",
		Error:     node.Error,
	}
	if len(node.StatusDetail) > 0 && json.Valid(node.StatusDetail) {
		response.StatusDetail = json.RawMessage(node.StatusDetail)
	}
	if operation != nil {
		response.Operation = &localFleetNodeOperation{
			Name:  operation.Operation,
			Phase: operation.Phase,
		}
	}
	return response
}

func localFleetDisplayStatus(node db.FleetNode, now time.Time) string {
	containerExists := node.ContainerID != "" && node.ObservedState != localFleetStateInstanceMissing
	containerState := ""
	switch node.ObservedState {
	case localFleetStateRunning:
		containerState = "running"
	case localFleetStateStopped, localFleetStateTerminated:
		containerState = "exited"
	}
	healthy := node.ObservedState == localFleetStateRunning
	healthFresh := node.HealthCheckedAt.Valid && now.Sub(node.HealthCheckedAt.Time) < localFleetHealthFreshWindow

	switch node.TargetState {
	case localFleetStateLaunching:
		return localFleetStateLaunching
	case localFleetStateStarting:
		if !containerExists {
			return localFleetStateInstanceMissing
		}
		if containerState == "running" && healthy && node.DaemonReady && healthFresh {
			return localFleetStateRunning
		}
		return localFleetStateStarting
	case localFleetStateRunning:
		if !containerExists {
			return localFleetStateInstanceMissing
		}
		if containerState == "exited" {
			return localFleetStateStopped
		}
		if containerState == "running" && healthy && node.DaemonReady && healthFresh {
			return localFleetStateRunning
		}
		return localFleetStateStarting
	case localFleetStateStopping:
		return localFleetStateStopping
	case localFleetStateStopped:
		return localFleetStateStopped
	case localFleetStateRebooting:
		return localFleetStateRebooting
	case localFleetStateTerminating:
		return localFleetStateTerminating
	case localFleetStateTerminated:
		return localFleetStateTerminated
	default:
		return localFleetStateFailed
	}
}

func localFleetNodeSpecID(raw []byte) string {
	var spec struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return ""
	}
	return spec.ID
}

func localFleetTimestamp(value pgtype.Timestamptz) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}
