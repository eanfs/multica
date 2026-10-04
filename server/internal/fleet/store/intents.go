package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Profile is private routing metadata; it must not be included in public DTOs.
type Profile struct {
	ID, OwnerID    pgtype.UUID
	Namespace, Ref string
	Version        int64
}

func validOwner(id pgtype.UUID) bool { return id.Valid && id.Bytes != [16]byte{} }
func validProfileRef(ref string) bool {
	return filepath.IsAbs(ref) && strings.TrimSpace(ref) != "" && !strings.ContainsRune(ref, 0)
}
func profileFromRow(row db.FleetCredentialProfile) Profile {
	return Profile{ID: row.ID, OwnerID: row.OwnerID, Namespace: row.Namespace, Ref: row.ProfileRef, Version: row.ConfigVersion}
}
func (s *Store) GetProfile(ctx context.Context, ownerID pgtype.UUID) (Profile, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	row, err := db.New(s.pool).GetFleetProfile(ctx, db.GetFleetProfileParams{Namespace: s.namespace, OwnerID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!validOwner(ownerID) || !validProfileRef(row.ProfileRef)) {
		return Profile{}, model.ErrProfileMissing
	}
	if err != nil {
		return Profile{}, err
	}
	return profileFromRow(row), nil
}

// GetProfileForNode checks the supplied identity against the persisted node before resolving routing.
// Read the resulting file only outside a database transaction.
func (s *Store) GetProfileForNode(ctx context.Context, node model.Node) (Profile, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	if node.Namespace != s.namespace || !validOwner(node.OwnerID) || !validOwner(node.ID) {
		return Profile{}, model.ErrProfileMissing
	}
	row, err := db.New(s.pool).GetFleetNode(ctx, db.GetFleetNodeParams{Namespace: s.namespace, OwnerID: node.OwnerID, NodeID: node.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, model.ErrProfileMissing
	}
	if err != nil {
		return Profile{}, err
	}
	p, err := s.GetProfile(ctx, node.OwnerID)
	if err != nil {
		return Profile{}, err
	}
	if row.ProfileRef == "" || row.ProfileRef != node.ProfileRef || row.ProfileRef != util.UUIDToString(p.ID) {
		return Profile{}, model.ErrProfileMissing
	}
	return p, nil
}

// UpsertProfiles is an atomic partial projection. Empty refs are retained disabled tombstones.
// Creation and projection acquire FleetOwnerExclusiveLock; batches lock canonical UUID order.
func (s *Store) UpsertProfiles(ctx context.Context, profiles map[pgtype.UUID]string, version int64) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	if len(profiles) == 0 {
		return nil
	}
	if version < 1 {
		return model.ErrConflict
	}
	owners := make([]pgtype.UUID, 0, len(profiles))
	for owner, ref := range profiles {
		if !validOwner(owner) || ref != "" && !validProfileRef(ref) {
			return model.ErrInvalidRequest
		}
		owners = append(owners, owner)
	}
	sort.Slice(owners, func(i, j int) bool { return util.UUIDToString(owners[i]) < util.UUIDToString(owners[j]) })
	return s.WithTx(ctx, func(q *db.Queries) error {
		for _, owner := range owners {
			if err := q.FleetOwnerExclusiveLock(ctx, db.FleetOwnerExclusiveLockParams{Namespace: s.namespace, OwnerID: owner}); err != nil {
				return err
			}
		}
		for _, owner := range owners {
			exists, err := q.FleetOwnerExists(ctx, owner)
			if err != nil {
				return err
			}
			if !exists {
				return model.ErrInvalidRequest
			}
			old, err := q.GetFleetProfile(ctx, db.GetFleetProfileParams{Namespace: s.namespace, OwnerID: owner})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil {
				if version < old.ConfigVersion || version == old.ConfigVersion && profiles[owner] != old.ProfileRef {
					return model.ErrConflict
				}
				if version == old.ConfigVersion {
					continue
				}
			}
			if err = q.UpsertFleetProfile(ctx, db.UpsertFleetProfileParams{Namespace: s.namespace, OwnerID: owner, ProfileRef: profiles[owner], ConfigVersion: version}); err != nil {
				return err
			}
		}
		return nil
	})
}

// CreateIntent locks owner before comparing identity, then validates current inputs only for NEW keys.
// No profile file or provider I/O is permitted here. Node and operation are committed together.
func (s *Store) CreateIntent(ctx context.Context, owner pgtype.UUID, req model.CreateRequest) (model.Node, model.Operation, bool, error) {
	return s.createIntent(ctx, owner, req, nil)
}

// CreateIntentForProfile admits only the trusted SQL snapshot validated outside the transaction.
// Matching durable replays win even when that snapshot is now stale or missing.
func (s *Store) CreateIntentForProfile(ctx context.Context, owner pgtype.UUID, req model.CreateRequest, profile Profile) (model.Node, model.Operation, bool, error) {
	return s.createIntent(ctx, owner, req, &profile)
}

func createFingerprint(req model.CreateRequest) string {
	raw, _ := json.Marshal(struct{ Name, Spec string }{req.Name, req.Spec})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// LookupCreateIntent is read-only and never checks current provisioning/profile inputs.
func (s *Store) LookupCreateIntent(ctx context.Context, owner pgtype.UUID, req model.CreateRequest) (model.Node, model.Operation, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	if !validOwner(owner) || strings.TrimSpace(req.IdempotencyKey) == "" {
		return model.Node{}, model.Operation{}, false, model.ErrInvalidRequest
	}
	return s.lookupCreateIntent(ctx, db.New(s.pool), owner, req, createFingerprint(req))
}

func (s *Store) lookupCreateIntent(ctx context.Context, q *db.Queries, owner pgtype.UUID, req model.CreateRequest, fingerprint string) (model.Node, model.Operation, bool, error) {
	existing, err := q.GetFleetIntentByKey(ctx, db.GetFleetIntentByKeyParams{Namespace: s.namespace, OwnerID: owner, IdempotencyKey: req.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Node{}, model.Operation{}, false, nil
	}
	if err != nil {
		return model.Node{}, model.Operation{}, false, err
	}
	if existing.RequestHash != fingerprint || existing.Action != "create" {
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

func (s *Store) createIntent(ctx context.Context, owner pgtype.UUID, req model.CreateRequest, admitted *Profile) (model.Node, model.Operation, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var node model.Node
	var op model.Operation
	replayed := false
	if !validOwner(owner) || strings.TrimSpace(req.IdempotencyKey) == "" {
		return node, op, false, model.ErrInvalidRequest
	}
	fingerprint := createFingerprint(req)
	err := s.WithTx(ctx, func(q *db.Queries) error {
		if err := q.FleetOwnerExclusiveLock(ctx, db.FleetOwnerExclusiveLockParams{Namespace: s.namespace, OwnerID: owner}); err != nil {
			return err
		}
		var err error
		node, op, replayed, err = s.lookupCreateIntent(ctx, q, owner, req, fingerprint)
		if err != nil || replayed {
			return err
		}
		if !s.validProvisioning() {
			return model.ErrInvalidRequest
		}
		if err = model.ValidateCreate(req, s.provisioning); err != nil {
			return err
		}
		exists, err := q.FleetOwnerExists(ctx, owner)
		if err != nil {
			return err
		}
		if !exists {
			return model.ErrInvalidRequest
		}
		profile, err := q.GetFleetProfile(ctx, db.GetFleetProfileParams{Namespace: s.namespace, OwnerID: owner})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !validProfileRef(profile.ProfileRef) {
			return model.ErrProfileMissing
		}
		if err != nil {
			return err
		}
		if admitted != nil && *admitted != profileFromRow(profile) {
			return model.ErrProfileMissing
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
		row, err := q.InsertFleetNode(ctx, db.InsertFleetNodeParams{Namespace: s.namespace, OwnerID: owner, Name: req.Name, Spec: req.Spec, Image: s.provisioning.Image, ProfileRef: util.UUIDToString(profile.ID), SpecConfig: snapshot})
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
