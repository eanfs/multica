package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"math"
	"strconv"
	"strings"
	"time"
)

func lifecycleFingerprint(node pgtype.UUID, action model.Action) string {
	raw, _ := json.Marshal(struct {
		Node   pgtype.UUID
		Action model.Action
	}{node, action})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func maintenanceAction(a model.Action) bool {
	return a == model.Stop || a == model.Reboot || a == model.Delete
}

// Start is a non-destructive approved intent: no report diagnosis or queue changes.
func (s *Store) CreateStartIntent(ctx context.Context, owner, node pgtype.UUID, key string) (model.Operation, error) {
	return s.lifecycleIntent(ctx, owner, node, model.Start, key)
}
func (s *Store) PrepareMaintenance(ctx context.Context, owner, node pgtype.UUID, action model.Action, key string) (model.Operation, error) {
	if !maintenanceAction(action) {
		return model.Operation{}, model.ErrInvalidRequest
	}
	return s.lifecycleIntent(ctx, owner, node, action, key)
}
func (s *Store) lockedNode(ctx context.Context, q *db.Queries, owner, node pgtype.UUID) (model.Node, error) {
	if e := q.FleetNodeExclusiveLock(ctx, db.FleetNodeExclusiveLockParams{Namespace: s.namespace, NodeID: node}); e != nil {
		return model.Node{}, e
	}
	row, e := q.GetFleetNode(ctx, db.GetFleetNodeParams{Namespace: s.namespace, OwnerID: owner, NodeID: node})
	if errors.Is(e, pgx.ErrNoRows) {
		return model.Node{}, model.ErrForbidden
	}
	if e != nil {
		return model.Node{}, e
	}
	return nodeFromRow(row)
}
func (s *Store) idleSQL(ctx context.Context, q *db.Queries, n model.Node, a model.Action) error {
	count, e := q.CountFleetActiveRuns(ctx, db.CountFleetActiveRunsParams{Namespace: s.namespace, OwnerID: n.OwnerID, NodeID: n.ID})
	if e != nil {
		return e
	}
	if count != 0 {
		return model.ErrBusy
	}
	if a == model.Delete {
		count, e = q.CountFleetQueuedRuns(ctx, db.CountFleetQueuedRunsParams{Namespace: s.namespace, OwnerID: n.OwnerID, NodeID: n.ID})
		if e != nil {
			return e
		}
		if count != 0 {
			return model.ErrBusy
		}
	}
	return nil
}
func (s *Store) lifecycleIntent(ctx context.Context, owner, node pgtype.UUID, action model.Action, key string) (model.Operation, error) {
	if !validOwner(owner) || !validOwner(node) || strings.TrimSpace(key) == "" || len(key) > 128 {
		return model.Operation{}, model.ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var op model.Operation
	e := s.WithTx(ctx, func(q *db.Queries) error {
		n, e := s.lockedNode(ctx, q, owner, node)
		if e != nil {
			return e
		}
		// Serialize owner-wide keys after the node fence. Creation never locks an existing node.
		if e = q.FleetOwnerExclusiveLock(ctx, db.FleetOwnerExclusiveLockParams{Namespace: s.namespace, OwnerID: owner}); e != nil {
			return e
		}
		hash := lifecycleFingerprint(node, action)
		old, e := q.GetFleetIntentByKey(ctx, db.GetFleetIntentByKeyParams{Namespace: s.namespace, OwnerID: owner, IdempotencyKey: key})
		if e == nil {
			if old.NodeID != node || old.Action != string(action) || old.RequestHash != hash {
				return model.ErrConflict
			}
			op = operationFromRow(old)
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if n.Revoked || n.Maintenance || n.Desired == "terminating" || n.Desired == "terminated" || n.Status == "terminating" || n.Status == "terminated" || n.Generation == math.MaxInt64 {
			return model.ErrConflict
		}
		unfinished, e := q.FleetUnfinishedOperations(ctx, db.FleetUnfinishedOperationsParams{Namespace: s.namespace, OwnerID: owner, NodeID: node})
		if e != nil {
			return e
		}
		if unfinished != 0 {
			return model.ErrConflict
		}
		phase, desired, approved := "preparing", n.Desired, false
		if action == model.Start {
			if n.Status != "stopped" && n.Status != "failed" && n.Status != "missing" {
				return model.ErrConflict
			}
			phase, desired, approved = "queued", "running", true
		} else if e = s.idleSQL(ctx, q, n, action); e != nil {
			return e
		}
		affected, e := q.FleetAdvanceIntent(ctx, db.FleetAdvanceIntentParams{Namespace: s.namespace, OwnerID: owner, NodeID: node, Generation: n.Generation, NextGeneration: n.Generation + 1, Desired: desired, Maintenance: action != model.Start})
		if e != nil {
			return e
		}
		if affected != 1 {
			return model.ErrConflict
		}
		row, e := q.InsertFleetLifecycleOperation(ctx, db.InsertFleetLifecycleOperationParams{Namespace: s.namespace, OwnerID: owner, NodeID: node, Action: string(action), IdempotencyKey: key, RequestHash: hash, Phase: phase, PriorDesired: n.Desired, Generation: n.Generation + 1, Approved: approved})
		if e != nil {
			return e
		}
		op = operationFromRow(row)
		return nil
	})
	if e != nil {
		return model.Operation{}, e
	}
	return op, nil
}

func (s *Store) maintenanceOperation(ctx context.Context, q *db.Queries, id pgtype.UUID, generation int64) (model.Node, model.Operation, error) {
	// Nonlocking identity discovery precedes the exclusive node fence; re-read follows it.
	locator, e := q.FleetOperationLocator(ctx, db.FleetOperationLocatorParams{Namespace: s.namespace, OperationID: id})
	if errors.Is(e, pgx.ErrNoRows) {
		return model.Node{}, model.Operation{}, model.ErrForbidden
	}
	if e != nil {
		return model.Node{}, model.Operation{}, e
	}
	n, e := s.lockedNode(ctx, q, locator.OwnerID, locator.NodeID)
	if e != nil {
		return n, model.Operation{}, e
	}
	row, e := q.GetFleetOperation(ctx, db.GetFleetOperationParams{Namespace: s.namespace, OwnerID: locator.OwnerID, OperationID: id})
	if e != nil {
		return n, model.Operation{}, e
	}
	op := operationFromRow(row)
	if row.NodeID != n.ID || op.Generation != generation || n.Generation != generation || !maintenanceAction(op.Action) || !n.Maintenance || n.Revoked {
		return n, op, model.ErrConflict
	}
	return n, op, nil
}

// Abort restores only this unapproved preparing operation; failed is a terminal phase.
func (s *Store) AbortMaintenance(ctx context.Context, id pgtype.UUID, generation int64) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	return s.WithTx(ctx, func(q *db.Queries) error {
		n, op, e := s.maintenanceOperation(ctx, q, id, generation)
		if e != nil {
			return e
		}
		if op.Approved || op.Phase != "preparing" {
			return model.ErrConflict
		}
		count, e := q.FleetAbortOperation(ctx, db.FleetAbortOperationParams{Namespace: s.namespace, OwnerID: op.OwnerID, OperationID: id, Generation: generation})
		if e != nil {
			return e
		}
		if count != 1 {
			return model.ErrConflict
		}
		count, e = q.FleetRestoreMaintenance(ctx, db.FleetRestoreMaintenanceParams{Namespace: s.namespace, OwnerID: n.OwnerID, NodeID: n.ID, Generation: generation, Desired: op.PriorDesired})
		if e != nil {
			return e
		}
		if count != 1 {
			return model.ErrConflict
		}
		return nil
	})
}

// MaintenanceSnapshot binds the supplied service-authenticated identity to SQL truth.
func (s *Store) MaintenanceSnapshot(ctx context.Context, owner pgtype.UUID, ref model.OperationRef) (model.Node, model.Operation, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var n model.Node
	var op model.Operation
	if ref.Namespace != s.namespace || !validOwner(owner) || !validOwner(ref.NodeID) || !validOwner(ref.OperationID) || ref.Generation < 1 || !maintenanceAction(ref.Action) {
		return n, op, model.ErrForbidden
	}
	e := s.WithTx(ctx, func(q *db.Queries) error {
		var e error
		n, e = s.lockedNode(ctx, q, owner, ref.NodeID)
		if e != nil {
			return e
		}
		row, e := q.GetFleetOperation(ctx, db.GetFleetOperationParams{Namespace: s.namespace, OwnerID: owner, OperationID: ref.OperationID})
		if errors.Is(e, pgx.ErrNoRows) {
			return model.ErrForbidden
		}
		if e != nil {
			return e
		}
		op = operationFromRow(row)
		if op.NodeID != n.ID || op.Generation != ref.Generation || n.Generation != ref.Generation || op.Action != ref.Action || !n.Maintenance {
			return model.ErrConflict
		}
		if op.Approved && op.Phase == "queued" {
			return nil
		}
		if op.Phase != "preparing" || op.Approved || n.Revoked {
			return model.ErrConflict
		}
		return nil
	})
	return n, op, e
}

// ApproveMaintenance validates explicit trusted diagnostic proof under the second exclusive fence.
// A known busy result commits the abort; unknown proof leaves the existing barrier intact.
func (s *Store) ApproveMaintenance(ctx context.Context, owner pgtype.UUID, ref model.OperationRef, baseline model.Node, observation model.Observation) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var outcome error
	if ref.Namespace != s.namespace || !validOwner(owner) || !validOwner(ref.NodeID) || !validOwner(ref.OperationID) {
		return model.ErrForbidden
	}
	e := s.WithTx(ctx, func(q *db.Queries) error {
		n, op, e := s.maintenanceOperation(ctx, q, ref.OperationID, ref.Generation)
		if e != nil {
			return e
		}
		if op.OwnerID != owner || op.NodeID != ref.NodeID || op.Action != ref.Action {
			return model.ErrForbidden
		}
		if op.Approved || op.Phase != "preparing" {
			return model.ErrConflict
		}
		if baseline.Namespace != n.Namespace || baseline.OwnerID != owner || baseline.ID != n.ID || baseline.Generation != n.Generation || baseline.ContainerID != n.ContainerID || baseline.StartEpoch != n.StartEpoch || baseline.DaemonID != n.DaemonID || baseline.DataVolume != n.DataVolume || baseline.SecretsVolume != n.SecretsVolume || baseline.Resources != n.Resources || baseline.Image != n.Image || baseline.ProfileRef != n.ProfileRef || !knownMaintenanceProof(n, op, observation, time.Now()) {
			return model.ErrUnknownHealth
		}
		if observation.ActiveRuns > 0 || observation.PendingReports > 0 || observation.FailedReports > 0 {
			e = model.ErrBusy
		}
		if e == nil {
			e = s.idleSQL(ctx, q, n, op.Action)
		}
		if errors.Is(e, model.ErrBusy) {
			count, e := q.FleetAbortOperation(ctx, db.FleetAbortOperationParams{Namespace: s.namespace, OwnerID: owner, OperationID: op.ID, Generation: op.Generation})
			if e != nil {
				return e
			}
			if count != 1 {
				return model.ErrConflict
			}
			count, e = q.FleetRestoreMaintenance(ctx, db.FleetRestoreMaintenanceParams{Namespace: s.namespace, OwnerID: owner, NodeID: n.ID, Generation: n.Generation, Desired: op.PriorDesired})
			if e != nil {
				return e
			}
			if count != 1 {
				return model.ErrConflict
			}
			outcome = model.ErrBusy
			return nil
		}
		if e != nil {
			return e
		}
		count, e := q.FleetApproveOperation(ctx, db.FleetApproveOperationParams{Namespace: s.namespace, OwnerID: owner, OperationID: op.ID, Generation: op.Generation})
		if e != nil {
			return e
		}
		if count != 1 {
			return model.ErrConflict
		}
		if op.Action == model.Delete {
			count, e = q.FleetApproveDelete(ctx, db.FleetApproveDeleteParams{Namespace: s.namespace, OwnerID: owner, NodeID: n.ID, Generation: n.Generation})
			if e != nil {
				return e
			}
			if count != 1 {
				return model.ErrConflict
			}
			if e = q.RevokeFleetCredentials(ctx, db.RevokeFleetCredentialsParams{Namespace: s.namespace, OwnerID: owner, NodeID: n.ID}); e != nil {
				return e
			}
		} else {
			desired := "running"
			if op.Action == model.Stop {
				desired = "stopped"
			}
			count, e = q.FleetSetMaintenanceDesired(ctx, db.FleetSetMaintenanceDesiredParams{Namespace: s.namespace, OwnerID: owner, NodeID: n.ID, Generation: n.Generation, Desired: desired})
			if e != nil {
				return e
			}
			if count != 1 {
				return model.ErrConflict
			}
		}
		return nil
	})
	if e != nil {
		return e
	}
	return outcome
}

func knownMaintenanceProof(n model.Node, op model.Operation, o model.Observation, now time.Time) bool {
	if !o.ReportStatsKnown || o.ObservedAt.IsZero() || o.ObservedAt.After(now) || now.Sub(o.ObservedAt) > 30*time.Second || o.ObservedAt.Before(op.CreatedAt) || o.RuntimeCount < 0 || o.ActiveRuns < 0 || o.PendingReports < 0 || o.FailedReports < 0 {
		return false
	}
	if o.Offline {
		return op.Action == model.Delete && (n.Status == "stopped" || n.Status == "missing") && (o.Status == "stopped" || o.Status == "missing") && n.DataVolume != "" && o.DataVolume == n.DataVolume && o.LayoutVersion == strconv.Itoa(model.LayoutVersion) && o.StartEpoch == "" && !o.Ready && o.ContainerID == "" && o.DaemonID == ""
	}
	return n.ContainerID != "" && n.StartEpoch != "" && n.DaemonID != "" && o.ContainerID == n.ContainerID && o.StartEpoch == n.StartEpoch && o.DaemonID == n.DaemonID && o.Status == "running" && o.Ready
}
