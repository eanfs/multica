package fleet

import (
	"context"
	"encoding/json"
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
	// coordinatorListLimit bounds one coordination pass. Nodes beyond the
	// first page are reconciled by a later pass.
	coordinatorListLimit = 200
	// defaultCoordinatorInterval matches the design's five-second cycle when a
	// caller passes a non-positive interval.
	defaultCoordinatorInterval = 5 * time.Second
)

// Tick is one coordination pass: refresh observed state for every node in the
// Fleet namespace, then execute approved operations. A single node or
// operation failure is recorded and does not abort the rest of the pass; only
// list failures are returned, because no progress is possible then.
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

	ops, err := s.store.Queries().ListApprovedFleetNodeOperationsByNamespace(ctx, s.cfg.Namespace)
	if err != nil {
		return fmt.Errorf("list approved operations: %w", err)
	}
	for _, op := range ops {
		if err := s.executeOperation(ctx, op); err != nil {
			s.recordOperationFailure(ctx, op, err)
		}
	}
	return nil
}

// RunCoordinator ticks until ctx is canceled. Tick errors are logged, not
// fatal: the next pass reconciles the same persisted state.
func (s *Service) RunCoordinator(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultCoordinatorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("fleet: coordinator tick failed", "error", err)
			}
		}
	}
}

// refreshNode reuses the status path so Inspect NotFound keeps becoming
// instance_missing while an unreachable daemon stays uncertain instead of
// clearing the node's state.
func (s *Service) refreshNode(ctx context.Context, node db.FleetNode) error {
	// Tombstones are final. Refreshing them would rewrite observed state to
	// instance_missing after the container and volumes were deleted.
	if node.TargetState == StateTerminated || node.TargetState == StateFailed {
		return nil
	}
	_, err := s.applyStatus(ctx, node)
	return err
}

// executeOperation applies one approved operation. The phase is persisted as
// executing before Docker is touched and as done only after the call
// succeeds, so a crash leaves an explicit recovery point.
func (s *Service) executeOperation(ctx context.Context, op db.FleetNodeOperation) error {
	node, err := s.store.Queries().GetFleetNode(ctx, db.GetFleetNodeParams{
		ID: op.NodeID, Namespace: s.cfg.Namespace,
	})
	if err != nil {
		return fmt.Errorf("load node for operation: %w", err)
	}
	if node.TargetState == StateTerminated {
		return fmt.Errorf("%w: node is already terminated", ErrConflict)
	}
	if err := s.markOperation(ctx, op, OpPhaseExecuting); err != nil {
		return err
	}
	switch op.Operation {
	case "stop":
		if node.ContainerID == "" {
			return fmt.Errorf("%w: node has no container to stop", ErrStateUncertain)
		}
		if err := s.provider.Stop(ctx, node.ContainerID, operationStopTimeout); err != nil {
			return fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
	case "start":
		if node.ContainerID == "" {
			return fmt.Errorf("%w: node has no container to start", ErrStateUncertain)
		}
		if err := s.provider.Start(ctx, node.ContainerID); err != nil {
			return fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
	case "reboot":
		if node.ContainerID == "" {
			return fmt.Errorf("%w: node has no container to reboot", ErrStateUncertain)
		}
		if err := s.provider.Restart(ctx, node.ContainerID, operationStopTimeout); err != nil {
			return fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
	case "delete":
		// A launching node may still be creating its container and volumes;
		// deleting it here would race the create and orphan resources.
		if node.TargetState == StateLaunching {
			return fmt.Errorf("%w: cannot delete a launching node", ErrNodeBusy)
		}
		if _, err := s.deleteNode(ctx, node); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unknown operation %q", ErrInvalidRequest, op.Operation)
	}
	return s.markOperation(ctx, op, OpPhaseDone)
}

func (s *Service) markOperation(ctx context.Context, op db.FleetNodeOperation, phase string) error {
	if err := s.store.Queries().UpdateFleetNodeOperationPhase(ctx, db.UpdateFleetNodeOperationPhaseParams{
		ID:         op.ID,
		Phase:      phase,
		ApprovedAt: op.ApprovedAt,
		RetryCount: op.RetryCount,
	}); err != nil {
		return fmt.Errorf("mark operation %s: %w", phase, err)
	}
	return nil
}

// recordOperationFailure moves the operation to failed and counts the attempt.
// The same operation id stays available for explicit recovery; the coordinator
// does not retry it on its own.
func (s *Service) recordOperationFailure(ctx context.Context, op db.FleetNodeOperation, cause error) {
	slog.Warn("fleet: operation failed",
		"operation", store.UUIDText(op.ID), "kind", op.Operation, "error", cause)
	if err := s.store.Queries().UpdateFleetNodeOperationPhase(ctx, db.UpdateFleetNodeOperationPhaseParams{
		ID:         op.ID,
		Phase:      OpPhaseFailed,
		ApprovedAt: op.ApprovedAt,
		RetryCount: op.RetryCount + 1,
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
