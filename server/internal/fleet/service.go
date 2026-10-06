package fleet

import (
	"context"
	"strconv"
	"sync"
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
	// auroraEnrollment is the process-local, one-time handoff from the Aurora
	// provision route to the Reconciler. Secrets are keyed by node UUID, never
	// persisted, and deleted on read; a Fleet restart drops them by design.
	auroraEnrollment sync.Map
}

func NewService(repo *store.Store, cfg model.Config, provider model.Provider) *Service {
	specs := make(map[string]model.Spec, len(cfg.Specs))
	for name, spec := range cfg.Specs {
		specs[name] = spec
	}
	cfg.Specs = specs
	return &Service{repo: repo, cfg: cfg, provider: provider}
}

// Create validates the administrator's private file outside every transaction.
// Replays are checked before admission, after failed admission, and again under the owner lock.
func (s *Service) Create(ctx context.Context, ownerID pgtype.UUID, req model.CreateRequest) (model.Node, model.Operation, bool, error) {
	if s.repo == nil {
		return model.Node{}, model.Operation{}, false, model.ErrUnavailable
	}
	node, op, replayed, err := s.repo.LookupCreateIntent(ctx, ownerID, req)
	if err != nil || replayed {
		return node, op, replayed, err
	}
	profile, err := s.repo.GetProfile(ctx, ownerID)
	if err == nil {
		_, err = LoadProfile(profile.Ref)
	}
	if err != nil {
		// A competing request may have committed while admission was checking the file.
		node, op, replayed, lookupErr := s.repo.LookupCreateIntent(ctx, ownerID, req)
		if lookupErr != nil || replayed {
			return node, op, replayed, lookupErr
		}
		return model.Node{}, model.Operation{}, false, err
	}
	return s.repo.CreateIntentForProfile(ctx, ownerID, req, profile)
}

// ProvisionAuroraNode admits one Aurora workspace node and registers its
// single-use enrollment secret in the process-local handoff table. An invalid
// secret fails closed before any row is admitted; the secret itself is never
// persisted and is deleted the moment the Reconciler takes it.
func (s *Service) ProvisionAuroraNode(ctx context.Context, ownerID, nodeID pgtype.UUID, req model.AuroraNodeRequest) (model.Node, model.Operation, error) {
	if s.repo == nil {
		return model.Node{}, model.Operation{}, model.ErrUnavailable
	}
	if !model.ValidEnrollmentToken(req.EnrollmentToken) {
		return model.Node{}, model.Operation{}, model.ErrInvalidRequest
	}
	node, op, _, err := s.repo.CreateAuroraIntent(ctx, ownerID, nodeID, req)
	if err != nil {
		return model.Node{}, model.Operation{}, err
	}
	// The handoff is a delivery channel, not an idempotency record. Aurora
	// re-arms a node with a fresh single-use secret under the same stable
	// idempotency key, so every call carrying a validated secret must (over)write
	// the entry, including a replay whose previous secret was consumed or expired.
	s.auroraEnrollment.Store(util.UUIDToString(nodeID), req.EnrollmentToken)
	return node, op, nil
}

// TakeAuroraEnrollment atomically consumes the one-time enrollment secret for
// one node. Missing means the Reconciler must fail the bootstrap the normal way;
// it never guesses success, mints a node token, or retries on a timer.
func (s *Service) TakeAuroraEnrollment(nodeID pgtype.UUID) (string, bool) {
	value, ok := s.auroraEnrollment.LoadAndDelete(util.UUIDToString(nodeID))
	if !ok {
		return "", false
	}
	token, ok := value.(string)
	return token, ok
}

// GetAuroraNode returns one owner- and namespace-scoped Aurora node.
func (s *Service) GetAuroraNode(ctx context.Context, ownerID, nodeID pgtype.UUID) (model.Node, error) {
	if s.repo == nil {
		return model.Node{}, model.ErrUnavailable
	}
	return s.repo.GetAuroraNode(ctx, ownerID, nodeID)
}

// DeleteAuroraNode records an approved destroy intent for the Reconciler; a
// missing or already-terminating node is success so Aurora cleanup is idempotent.
func (s *Service) DeleteAuroraNode(ctx context.Context, ownerID, nodeID pgtype.UUID) error {
	if s.repo == nil {
		return model.ErrUnavailable
	}
	return s.repo.DeleteAuroraIntent(ctx, ownerID, nodeID)
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
