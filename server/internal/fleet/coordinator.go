package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/multica-ai/multica/server/internal/fleet/docker"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Operation phases. The API writes prepared and approves; the coordinator only
// executes approved rows and never invents approval.
const (
	OpPhasePrepared  = "prepared"
	OpPhaseApproved  = "approved"
	OpPhaseExecuting = "executing"
	OpPhaseDone      = "done"
	OpPhaseFailed    = "failed"
	OpPhaseAborted   = "aborted"
)

const (
	// coordinatorListLimit bounds one node page in a coordination pass. Nodes
	// beyond the first page are reconciled by a later pass.
	coordinatorListLimit = 200
	// defaultCoordinatorInterval matches the design's five-second cycle when a
	// caller passes a non-positive interval.
	defaultCoordinatorInterval = 5 * time.Second
	// maxOperationRetries bounds automatic retries: the fifth failure parks the
	// operation as failed instead of requeueing it again.
	maxOperationRetries = 5
)

// errOperationDeferred reports that an operation cannot run yet but has not
// failed. The coordinator leaves the row in its current phase so a later pass
// can consume it instead of counting a retry.
var errOperationDeferred = errors.New("fleet operation deferred")

// Tick is one coordination pass: refresh observed state for every node in the
// Fleet namespace, then execute runnable operations. A single node or operation
// failure is recorded and does not abort the rest of the pass; only list
// failures are returned, because no progress is possible then.
func (s *Service) Tick(ctx context.Context) error {
	if s == nil || s.store == nil || s.provider == nil {
		return ErrNotConfigured
	}
	// Page through every node so a large namespace cannot starve the newest
	// rows behind a single LIMIT.
	for offset := 0; ; offset += coordinatorListLimit {
		nodes, err := s.store.Queries().ListFleetNodesByNamespace(ctx, db.ListFleetNodesByNamespaceParams{
			Namespace:   s.cfg.Namespace,
			LimitCount:  coordinatorListLimit,
			OffsetCount: int32(offset),
		})
		if err != nil {
			return fmt.Errorf("list fleet nodes: %w", err)
		}
		for _, node := range nodes {
			if err := s.refreshNode(ctx, node); err != nil {
				slog.Warn("fleet: refresh node failed",
					"node", store.UUIDText(node.ID), "error", err)
			}
		}
		if len(nodes) < coordinatorListLimit {
			break
		}
	}

	ops, err := s.store.Queries().ListRunnableFleetNodeOperationsByNamespace(ctx, s.cfg.Namespace)
	if err != nil {
		return fmt.Errorf("list runnable operations: %w", err)
	}
	for _, op := range ops {
		if err := s.executeOperation(ctx, op); err != nil {
			if errors.Is(err, errOperationDeferred) {
				slog.Debug("fleet: operation deferred",
					"operation", store.UUIDText(op.ID), "kind", op.Operation, "reason", err)
				continue
			}
			s.recordOperationFailure(ctx, op, err)
		}
	}
	return nil
}

// RunCoordinator reconciles immediately and then ticks until ctx is canceled.
// Tick errors are logged, not fatal: the next pass reconciles the same
// persisted state.
func (s *Service) RunCoordinator(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultCoordinatorInterval
	}
	tick := func() {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("fleet: coordinator tick failed", "error", err)
		}
	}
	tick()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// refreshNode reuses the status path so Inspect NotFound keeps becoming
// instance_missing while an unreachable daemon stays uncertain instead of
// clearing the node's state. It then advances a transitional target state that
// the observation has settled.
func (s *Service) refreshNode(ctx context.Context, node db.FleetNode) error {
	// Tombstones are final. Refreshing them would rewrite observed state to
	// instance_missing after the container and volumes were deleted.
	if node.TargetState == StateTerminated || node.TargetState == StateFailed {
		return nil
	}
	obs, err := s.observeNode(ctx, node)
	if err != nil {
		return err
	}
	return s.advanceTargetState(ctx, node, obs)
}

// advanceTargetState persists a settled target state exactly once. Writing it
// on every tick would bump generation without any state change, which would
// break the claim barriers that compare generation.
func (s *Service) advanceTargetState(ctx context.Context, node db.FleetNode, obs nodeObservation) error {
	next, changed := nextTargetState(node.TargetState, obs)
	if !changed {
		return nil
	}
	if err := s.store.Queries().UpdateFleetNodeTargetState(ctx, db.UpdateFleetNodeTargetStateParams{
		ID:          node.ID,
		Namespace:   s.cfg.Namespace,
		TargetState: next,
	}); err != nil {
		return fmt.Errorf("advance node target state: %w", err)
	}
	return nil
}

// nextTargetState reports the settled target state a transitional node reached.
// It only advances starting/rebooting to running and stopping to stopped. A
// running node whose container exited stays running: only an explicit start may
// move it back to starting. DeriveDisplayState keeps transitional targets
// transitional, so the observation compares the settled display instead.
func nextTargetState(target string, obs nodeObservation) (string, bool) {
	if !obs.exists {
		return "", false
	}
	switch target {
	case StateStarting, StateRebooting:
		if obs.settled == StateRunning {
			return StateRunning, true
		}
	case StateStopping:
		if obs.settled == StateStopped {
			return StateStopped, true
		}
	}
	return "", false
}

// executeOperation applies one runnable operation. Guards run before the row is
// claimed, so a node that cannot accept the operation yet does not consume it.
// The phase is persisted as executing before Docker is touched and as done only
// after the call succeeds, so a crash leaves an explicit recovery point.
func (s *Service) executeOperation(ctx context.Context, op db.FleetNodeOperation) error {
	node, err := s.store.Queries().GetFleetNode(ctx, db.GetFleetNodeParams{
		ID: op.NodeID, Namespace: s.cfg.Namespace,
	})
	if err != nil {
		return fmt.Errorf("load node for operation: %w", err)
	}
	if err := operationGuard(node, op.Operation); err != nil {
		return err
	}
	claimed, err := s.claimOperation(ctx, op)
	if err != nil {
		return err
	}
	if !claimed {
		// Another coordinator instance moved the row first; leave it alone.
		slog.Debug("fleet: operation already claimed",
			"operation", store.UUIDText(op.ID), "kind", op.Operation)
		return nil
	}

	dockerCtx, cancel := context.WithTimeout(ctx, dockerCallTimeout)
	defer cancel()
	switch op.Operation {
	case "stop":
		if err := s.provider.Stop(dockerCtx, node.ContainerID, operationStopTimeout); err != nil {
			return fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
	case "start":
		if err := s.provider.Start(dockerCtx, node.ContainerID); err != nil {
			return fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
	case "reboot":
		if err := s.provider.Restart(dockerCtx, node.ContainerID, operationStopTimeout); err != nil {
			return fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
	case "delete":
		if _, err := s.deleteNode(ctx, node); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unknown operation %q", ErrInvalidRequest, op.Operation)
	}
	return s.completeOperation(ctx, op)
}

// operationGuard rejects operations that cannot safely run yet without treating
// them as failures. A launching node may still be creating its container and
// volumes, and a terminated node has no container left, so consuming such an
// operation would fail an approval that is only waiting for the node to settle.
func operationGuard(node db.FleetNode, operation string) error {
	if node.TargetState == StateTerminated {
		return fmt.Errorf("%w: node is already terminated", errOperationDeferred)
	}
	if node.TargetState == StateLaunching {
		return fmt.Errorf("%w: node is still launching", errOperationDeferred)
	}
	switch operation {
	case "stop", "start", "reboot":
		if node.ContainerID == "" {
			return fmt.Errorf("%w: node has no container", errOperationDeferred)
		}
	case "delete":
		// A failed launch has no container; delete still revokes the
		// credential and removes the volumes, so it stays runnable.
	default:
		return fmt.Errorf("%w: unknown operation %q", ErrInvalidRequest, operation)
	}
	return nil
}

// claimOperation moves a runnable row to executing only when it is still in the
// phase that made it runnable. affected == 0 means another instance won.
func (s *Service) claimOperation(ctx context.Context, op db.FleetNodeOperation) (bool, error) {
	affected, err := s.store.Queries().MarkFleetNodeOperationPhase(ctx, db.MarkFleetNodeOperationPhaseParams{
		ID:            op.ID,
		Phase:         OpPhaseExecuting,
		ExpectedPhase: op.Phase,
	})
	if err != nil {
		return false, fmt.Errorf("claim operation %s: %w", store.UUIDText(op.ID), err)
	}
	return affected == 1, nil
}

// completeOperation moves the claimed row to done. Only the phase changes, so a
// message from an earlier failed attempt is not silently cleared.
func (s *Service) completeOperation(ctx context.Context, op db.FleetNodeOperation) error {
	affected, err := s.store.Queries().MarkFleetNodeOperationPhase(ctx, db.MarkFleetNodeOperationPhaseParams{
		ID:            op.ID,
		Phase:         OpPhaseDone,
		ExpectedPhase: OpPhaseExecuting,
	})
	if err != nil {
		return fmt.Errorf("mark operation done: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: operation is no longer executing", ErrConflict)
	}
	return nil
}

// recordOperationFailure counts one failed attempt. Attempts below the retry
// limit return the row to approved so the next pass tries again; the last
// attempt parks it as failed with the error that stopped it. Only the columns
// this path owns are written, so an earlier error message is never blanked.
func (s *Service) recordOperationFailure(ctx context.Context, op db.FleetNodeOperation, cause error) {
	retryCount := op.RetryCount + 1
	phase := OpPhaseApproved
	if retryCount >= maxOperationRetries {
		phase = OpPhaseFailed
	}
	slog.Warn("fleet: operation failed",
		"operation", store.UUIDText(op.ID), "kind", op.Operation,
		"phase", phase, "retry_count", retryCount, "error", cause)
	if err := s.store.Queries().RecordFleetNodeOperationFailure(ctx, db.RecordFleetNodeOperationFailureParams{
		ID:         op.ID,
		Phase:      phase,
		RetryCount: retryCount,
		Error:      sanitize(cause),
	}); err != nil {
		slog.Warn("fleet: record operation failure failed",
			"operation", store.UUIDText(op.ID), "error", err)
	}
}

// statusDetailFromHealth turns one daemon health sample into the separate
// readiness signals the UI shows. Credential validity is never inferred from a
// healthy daemon: Fleet does not probe model credentials by default, so the
// flag stays false until an explicit check exists.
func statusDetailFromHealth(report docker.HealthReport) []byte {
	detail, err := json.Marshal(map[string]any{
		"cli_installed":     containsAgent(report.Agents, "claude"),
		"model_configured":  report.Status == "running",
		"credentials_valid": false,
	})
	if err != nil {
		return []byte("{}")
	}
	return detail
}

func containsAgent(agents []string, want string) bool {
	for _, agent := range agents {
		if agent == want {
			return true
		}
	}
	return false
}
