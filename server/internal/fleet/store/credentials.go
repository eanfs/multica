package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// NewNodeToken is a leaf helper: plaintext is returned only for private bootstrap use.
func NewNodeToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	token = "mcn_" + base64.RawURLEncoding.EncodeToString(raw)
	return token, auth.HashToken(token), nil
}
func (s *Store) credentialNode(ctx context.Context, q *db.Queries, nodeID pgtype.UUID) (db.FleetNode, error) {
	if !validOwner(nodeID) {
		return db.FleetNode{}, model.ErrForbidden
	}
	if err := q.FleetNodeExclusiveLock(ctx, db.FleetNodeExclusiveLockParams{Namespace: s.namespace, NodeID: nodeID}); err != nil {
		return db.FleetNode{}, err
	}
	node, err := q.GetFleetNodeByID(ctx, db.GetFleetNodeByIDParams{Namespace: s.namespace, NodeID: nodeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.FleetNode{}, model.ErrForbidden
	}
	if err != nil {
		return node, err
	}
	exists, err := q.FleetOwnerExists(ctx, node.OwnerID)
	if err != nil {
		return node, err
	}
	if !exists || !validOwner(node.OwnerID) {
		return db.FleetNode{}, model.ErrForbidden
	}
	return node, nil
}

// MintNodeToken revokes predecessors and persists max(history)+1 under a node-exclusive lock.
// Credential generation never advances the node/operation control CAS generation.
func (s *Store) MintNodeToken(ctx context.Context, nodeID pgtype.UUID) (string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	token, hash, err := NewNodeToken()
	if err != nil {
		return "", 0, err
	}
	var generation int64
	err = s.WithTx(ctx, func(q *db.Queries) error {
		node, err := s.credentialNode(ctx, q, nodeID)
		if err != nil {
			return err
		}
		if node.Revoked || node.Desired == "terminated" || node.Status == "terminated" {
			return model.ErrForbidden
		}
		generation, err = q.MaxFleetCredentialGeneration(ctx, db.MaxFleetCredentialGenerationParams{Namespace: s.namespace, NodeID: nodeID, OwnerID: node.OwnerID})
		if err != nil {
			return err
		}
		if generation == math.MaxInt64 {
			return model.ErrUnavailable
		}
		generation++
		if err = q.RevokeFleetCredentials(ctx, db.RevokeFleetCredentialsParams{Namespace: s.namespace, NodeID: nodeID, OwnerID: node.OwnerID}); err != nil {
			return err
		}
		return q.InsertFleetCredential(ctx, db.InsertFleetCredentialParams{Namespace: s.namespace, NodeID: nodeID, OwnerID: node.OwnerID, TokenHash: hash, Generation: generation})
	})
	if err != nil {
		return "", 0, err
	}
	return token, generation, nil
}
func (s *Store) VerifyNodeToken(ctx context.Context, token string) (model.Node, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "mcn_"))
	if !strings.HasPrefix(token, "mcn_") || len(token) != 47 || err != nil || len(raw) != 32 {
		return model.Node{}, model.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	node, err := db.New(s.pool).VerifyFleetCredential(ctx, db.VerifyFleetCredentialParams{Namespace: s.namespace, TokenHash: auth.HashToken(token)})
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Node{}, model.ErrForbidden
	}
	if err != nil {
		return model.Node{}, err
	}
	return nodeFromRow(node)
}
func (s *Store) RevokeNodeToken(ctx context.Context, nodeID pgtype.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	return s.WithTx(ctx, func(q *db.Queries) error {
		node, err := s.credentialNode(ctx, q, nodeID)
		if err != nil {
			return err
		}
		return q.RevokeFleetCredentials(ctx, db.RevokeFleetCredentialsParams{Namespace: s.namespace, NodeID: nodeID, OwnerID: node.OwnerID})
	})
}
