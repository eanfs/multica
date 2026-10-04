package fleet

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type Service struct {
	repo     *store.Store
	cfg      model.Config
	provider model.Provider
}

func NewService(repo *store.Store, cfg model.Config, provider model.Provider) *Service {
	return &Service{repo: repo, cfg: cfg, provider: provider}
}

// Status returns the controller-persisted actual snapshot, not the desired state or a provider secret.
func (s *Service) Status(ctx context.Context, ownerID pgtype.UUID, reference string) (model.Node, error) {
	if reference == "" {
		return model.Node{}, model.ErrInvalidRequest
	}
	if s.repo == nil {
		return model.Node{}, model.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var nodeID pgtype.UUID
	e := s.repo.WithTx(ctx, func(q *db.Queries) error {
		id, e := q.ResolveFleetNodeReference(ctx, db.ResolveFleetNodeReferenceParams{Namespace: s.cfg.Namespace, OwnerID: ownerID, Reference: reference})
		nodeID = id
		return e
	})
	if e != nil {
		return model.Node{}, e
	}
	return s.repo.GetNode(ctx, ownerID, nodeID)
}

// operationNode checks persisted identities under the namespaced node lock. No provider I/O runs here.
func (s *Service) operationNode(ctx context.Context, ownerID pgtype.UUID, ref model.OperationRef, approved bool) (model.Node, model.Operation, error) {
	if s.repo == nil {
		return model.Node{}, model.Operation{}, model.ErrUnavailable
	}
	if ref.Namespace != s.cfg.Namespace {
		return model.Node{}, model.Operation{}, model.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, e := s.repo.GetNode(ctx, ownerID, ref.NodeID)
	if e != nil {
		return n, model.Operation{}, e
	}
	var op model.Operation
	e = s.repo.WithTx(ctx, func(q *db.Queries) error {
		if e := q.FleetNodeSharedLock(ctx, db.FleetNodeSharedLockParams{Namespace: ref.Namespace, NodeID: ref.NodeID}); e != nil {
			return e
		}
		row, e := q.GetFleetNode(ctx, db.GetFleetNodeParams{Namespace: ref.Namespace, OwnerID: ownerID, NodeID: ref.NodeID})
		if e != nil {
			return e
		}
		o, e := q.GetFleetOperation(ctx, db.GetFleetOperationParams{Namespace: ref.Namespace, OwnerID: ownerID, OperationID: ref.OperationID})
		if e != nil {
			return e
		}
		if o.NodeID != ref.NodeID || o.Generation != ref.Generation || row.Generation != ref.Generation || model.Action(o.Action) != ref.Action || (approved && !o.Approved) {
			return model.ErrConflict
		}
		if n.Generation != row.Generation || n.ContainerID != row.ContainerID || n.StartEpoch != row.StartEpoch || n.DataVolume != row.DataVolume || n.DaemonID != util.UUIDToString(row.DaemonID) || !n.UpdatedAt.Equal(row.UpdatedAt.Time) {
			return model.ErrConflict
		}
		op = model.Operation{ID: o.ID, NodeID: o.NodeID, OwnerID: o.OwnerID, Action: model.Action(o.Action), Phase: o.Phase, Generation: o.Generation, Approved: o.Approved, IdempotencyKey: o.IdempotencyKey, CreatedAt: o.CreatedAt.Time, UpdatedAt: o.UpdatedAt.Time}
		return nil
	})
	return n, op, e
}

// AcceptOperation acknowledges durable, approved work. The controller, never HTTP, applies it.
func (s *Service) AcceptOperation(ctx context.Context, ownerID pgtype.UUID, ref model.OperationRef, key string) (model.Node, model.Operation, error) {
	n, op, e := s.operationNode(ctx, ownerID, ref, true)
	if e != nil {
		return n, op, e
	}
	if key == "" || op.IdempotencyKey != key || op.Phase == "failed" {
		return n, op, model.ErrConflict
	}
	return n, op, nil
}

// Diagnose is fixed-purpose, read-only and bounded; the operation is rechecked after external I/O.
func (s *Service) Diagnose(ctx context.Context, ownerID pgtype.UUID, ref model.OperationRef) (model.Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, _, e := s.operationNode(ctx, ownerID, ref, false)
	if e != nil {
		return model.Observation{}, e
	}
	if s.provider == nil {
		return model.Observation{}, model.ErrUnavailable
	}
	o, e := s.provider.Diagnose(ctx, n, ref)
	if e != nil {
		return model.Observation{}, model.ErrUnavailable
	}
	latest, _, e := s.operationNode(ctx, ownerID, ref, false)
	if e != nil {
		return model.Observation{}, e
	}
	if latest.StartEpoch != n.StartEpoch || latest.ContainerID != n.ContainerID || latest.DaemonID != n.DaemonID || latest.DataVolume != n.DataVolume {
		return model.Observation{}, model.ErrConflict
	}
	now := time.Now()
	if o.ObservedAt.IsZero() || o.ObservedAt.After(now) || now.Sub(o.ObservedAt) > 30*time.Second || o.RuntimeCount < 0 || o.ActiveRuns < 0 || o.PendingReports < 0 || o.FailedReports < 0 {
		return model.Observation{}, model.ErrUnknownHealth
	}
	if o.Offline {
		if n.DataVolume == "" || o.DataVolume != n.DataVolume || o.LayoutVersion != strconv.Itoa(model.LayoutVersion) || o.StartEpoch != "" {
			return model.Observation{}, model.ErrUnknownHealth
		}
		o.Ready = false
	} else if o.ContainerID != n.ContainerID || o.DaemonID != n.DaemonID || o.StartEpoch != n.StartEpoch {
		return model.Observation{}, model.ErrUnknownHealth
	}
	if !o.ReportStatsKnown {
		o.Ready = false
	}
	return o, nil
}
