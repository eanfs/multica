package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// auroraFingerprint binds one idempotency key to the immutable identity of an
// Aurora node intent. The one-time enrollment secret is deliberately excluded:
// a rotated secret must still replay the original node and operation.
func auroraFingerprint(nodeID, workspaceID, runtimeID pgtype.UUID, req model.AuroraNodeRequest) string {
	raw, _ := json.Marshal(struct {
		NodeID, WorkspaceID, RuntimeID    pgtype.UUID
		DaemonID, ImageDigest, Name, Spec string
	}{nodeID, workspaceID, runtimeID, req.DaemonID, req.ImageDigest, req.Name, req.Spec})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// validAurora admits only an administrator-approved image and declared spec. The
// caller cannot smuggle resources, a profile path or another image through the
// request. DaemonID must be a canonical UUID; its text form is persisted.
func (s *Store) validAurora(nodeID pgtype.UUID, req model.AuroraNodeRequest) bool {
	// The Aurora profile gate lives at the HTTP/Service layer (the route only
	// exists under Config.Aurora); the store still validates the administrator's
	// approved image and declared spec so a caller cannot smuggle either.
	if !validOwner(nodeID) || !validOwner(req.WorkspaceID) || !validOwner(req.RuntimeID) || !s.validProvisioning() {
		return false
	}
	if req.ImageDigest != s.provisioning.Image {
		return false
	}
	if _, err := util.ParseUUID(req.DaemonID); err != nil {
		return false
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 128 {
		return false
	}
	return model.ValidateCreate(model.CreateRequest{Name: req.Name, Spec: req.Spec}, s.provisioning) == nil
}

func (s *Store) lookupAuroraIntent(ctx context.Context, q *db.Queries, owner, nodeID pgtype.UUID, req model.AuroraNodeRequest, fingerprint string) (model.Node, model.Operation, bool, error) {
	existing, err := q.GetFleetIntentByKey(ctx, db.GetFleetIntentByKeyParams{Namespace: s.namespace, OwnerID: owner, IdempotencyKey: req.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Node{}, model.Operation{}, false, nil
	}
	if err != nil {
		return model.Node{}, model.Operation{}, false, err
	}
	// The key must not have been used for another node, action or payload.
	if existing.RequestHash != fingerprint || existing.Action != "create" || existing.NodeID != nodeID {
		return model.Node{}, model.Operation{}, false, model.ErrConflict
	}
	row, err := q.GetFleetNode(ctx, db.GetFleetNodeParams{Namespace: s.namespace, OwnerID: owner, NodeID: existing.NodeID})
	if err != nil {
		return model.Node{}, model.Operation{}, false, err
	}
	node, err := nodeFromRow(row)
	if err != nil {
		return model.Node{}, model.Operation{}, false, err
	}
	return node, operationFromRow(existing), true, nil
}

// CreateAuroraIntent is the single Aurora workspace-node admission point. The
// caller owns the node and daemon UUIDs; Fleet validates owner/namespace/image/
// spec, then commits the node and create operation atomically under the owner
// lock. A matching replay returns the original node/operation even after quota
// or configuration changes.
func (s *Store) CreateAuroraIntent(ctx context.Context, owner, nodeID pgtype.UUID, req model.AuroraNodeRequest) (model.Node, model.Operation, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var node model.Node
	var op model.Operation
	replayed := false
	if !validOwner(owner) || !validOwner(nodeID) || strings.TrimSpace(req.IdempotencyKey) == "" {
		return node, op, false, model.ErrInvalidRequest
	}
	fingerprint := auroraFingerprint(nodeID, req.WorkspaceID, req.RuntimeID, req)
	err := s.WithTx(ctx, func(q *db.Queries) error {
		admission := CheckNamespaceAdmission(ctx, q, s.namespace)
		if admission != nil && !errors.Is(admission, model.ErrBusy) {
			return admission
		}
		if err := q.FleetOwnerExclusiveLock(ctx, db.FleetOwnerExclusiveLockParams{Namespace: s.namespace, OwnerID: owner}); err != nil {
			return err
		}
		var err error
		node, op, replayed, err = s.lookupAuroraIntent(ctx, q, owner, nodeID, req, fingerprint)
		if err != nil || replayed {
			return err
		}
		if admission != nil {
			return admission
		}
		if !s.validAurora(nodeID, req) {
			return model.ErrInvalidRequest
		}
		exists, err := q.FleetOwnerExists(ctx, owner)
		if err != nil {
			return err
		}
		if !exists {
			return model.ErrInvalidRequest
		}
		count, err := q.CountFleetProvisionedNodes(ctx, db.CountFleetProvisionedNodesParams{Namespace: s.namespace, OwnerID: owner})
		if err != nil {
			return err
		}
		if count >= int64(s.maxNodes) {
			return model.ErrConflict
		}
		snapshot, err := json.Marshal(s.provisioning.Specs[req.Spec])
		if err != nil {
			return model.ErrInvalidRequest
		}
		daemonID, _ := util.ParseUUID(req.DaemonID)
		nodeText := util.UUIDToString(nodeID)
		row, err := q.InsertFleetAuroraNode(ctx, db.InsertFleetAuroraNodeParams{NodeID: nodeID, DaemonID: daemonID, Namespace: s.namespace, OwnerID: owner, WorkspaceID: req.WorkspaceID, RuntimeID: req.RuntimeID, Name: req.Name, Spec: req.Spec, Image: req.ImageDigest, SpecConfig: snapshot, DataVolume: "multica-fleet-" + nodeText + "-data", SecretsVolume: "multica-fleet-" + nodeText + "-secrets"})
		if err != nil {
			return err
		}
		node, err = nodeFromRow(row)
		if err != nil {
			return err
		}
		operation, err := q.InsertFleetCreateOperation(ctx, db.InsertFleetCreateOperationParams{Namespace: s.namespace, OwnerID: owner, NodeID: node.ID, IdempotencyKey: req.IdempotencyKey, RequestHash: fingerprint})
		if err != nil {
			return err
		}
		op = operationFromRow(operation)
		return nil
	})
	if err != nil {
		return model.Node{}, model.Operation{}, false, err
	}
	return node, op, replayed, nil
}

// GetAuroraNode reads one owner- and namespace-scoped node. A missing, foreign
// owner or foreign namespace is pgx.ErrNoRows, never another owner's node.
func (s *Store) GetAuroraNode(ctx context.Context, owner, nodeID pgtype.UUID) (model.Node, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	if !validOwner(owner) || !validOwner(nodeID) {
		return model.Node{}, model.ErrForbidden
	}
	row, err := db.New(s.pool).GetFleetNode(ctx, db.GetFleetNodeParams{Namespace: s.namespace, OwnerID: owner, NodeID: nodeID})
	if err != nil {
		return model.Node{}, err
	}
	return nodeFromRow(row)
}

// DeleteAuroraIntent records one approved destroy intent and lets the
// Reconciler perform the physical deletion. It is idempotent: a node that is
// already gone or already terminating is success, because Aurora retries
// cleanup. The node lock serializes concurrent deletes under one key.
func (s *Store) DeleteAuroraIntent(ctx context.Context, owner, nodeID pgtype.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	if !validOwner(owner) || !validOwner(nodeID) {
		return model.ErrForbidden
	}
	return s.WithTx(ctx, func(q *db.Queries) error {
		admission := CheckNamespaceAdmission(ctx, q, s.namespace)
		if admission != nil && !errors.Is(admission, model.ErrBusy) {
			return admission
		}
		n, err := s.lockedNode(ctx, q, owner, nodeID)
		if errors.Is(err, model.ErrForbidden) {
			return nil
		}
		if err != nil {
			return err
		}
		if n.Desired == "terminating" || n.Desired == "terminated" || n.Status == "terminated" {
			return nil
		}
		if admission != nil {
			return admission
		}
		// An Aurora node may still hold an unfinished create operation when its
		// daemon never enrolled; delete is the cleanup for exactly that case, so
		// quiescence is not required. An already-revoked node is also deletable.
		if n.Maintenance || n.Generation == math.MaxInt64 {
			return model.ErrConflict
		}
		next := n.Generation + 1
		advanced, err := q.FleetAdvanceIntent(ctx, db.FleetAdvanceIntentParams{Namespace: s.namespace, OwnerID: owner, NodeID: nodeID, Generation: n.Generation, NextGeneration: next, Desired: "terminating", Maintenance: true})
		if err != nil {
			return err
		}
		if advanced != 1 {
			return model.ErrConflict
		}
		approved, err := q.FleetApproveDelete(ctx, db.FleetApproveDeleteParams{Namespace: s.namespace, OwnerID: owner, NodeID: nodeID, Generation: next})
		if err != nil {
			return err
		}
		if approved != 1 {
			return model.ErrConflict
		}
		if err := q.RevokeFleetCredentials(ctx, db.RevokeFleetCredentialsParams{Namespace: s.namespace, OwnerID: owner, NodeID: nodeID}); err != nil {
			return err
		}
		_, err = q.InsertFleetLifecycleOperation(ctx, db.InsertFleetLifecycleOperationParams{Namespace: s.namespace, OwnerID: owner, NodeID: nodeID, Action: string(model.Delete), IdempotencyKey: "aurora-delete:" + util.UUIDToString(nodeID), RequestHash: lifecycleFingerprint(nodeID, model.Delete), Phase: "queued", PriorDesired: n.Desired, Generation: next, Approved: true})
		return err
	})
}
