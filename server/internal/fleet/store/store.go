package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrLimitReached reports that the owner already has the maximum number of
// fleet nodes allowed in the namespace.
var ErrLimitReached = errors.New("fleet node limit reached")

// Store owns the PostgreSQL transaction boundaries for fleet state.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Queries returns the generated queries bound to the pool.
func (s *Store) Queries() *db.Queries { return db.New(s.pool) }

// WithinTx runs fn in a transaction and commits only when fn returns nil.
// Errors from fn are returned unchanged so callers can match sentinel values.
func (s *Store) WithinTx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fleet tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit fleet tx: %w", err)
	}
	return nil
}

// ReserveNode creates a node when the owner is below maxPerOwner. The
// transaction-scoped advisory lock serializes concurrent reservations for one
// owner in one namespace, so the count check and insert cannot race.
func (s *Store) ReserveNode(ctx context.Context, p db.CreateFleetNodeParams, maxPerOwner int) (db.FleetNode, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("begin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))",
		"fleet-owner:"+p.Namespace+":"+UUIDText(p.OwnerID)); err != nil {
		return db.FleetNode{}, fmt.Errorf("lock fleet owner: %w", err)
	}
	q := db.New(tx)
	count, err := q.CountFleetNodesByOwner(ctx, db.CountFleetNodesByOwnerParams{
		OwnerID: p.OwnerID, Namespace: p.Namespace,
	})
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("count fleet nodes: %w", err)
	}
	if count >= int64(maxPerOwner) {
		return db.FleetNode{}, ErrLimitReached
	}
	node, err := q.CreateFleetNode(ctx, p)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("create fleet node: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.FleetNode{}, fmt.Errorf("commit reserve: %w", err)
	}
	return node, nil
}

// RevokeCredentials marks every active credential for the node as revoked.
func (s *Store) RevokeCredentials(ctx context.Context, nodeID pgtype.UUID) error {
	return s.Queries().RevokeFleetNodeCredentialsByNode(ctx, nodeID)
}

// ListStaleNodes returns namespace nodes whose health check is missing or older
// than the query's staleness window.
func (s *Store) ListStaleNodes(ctx context.Context, namespace string) ([]db.FleetNode, error) {
	return s.Queries().ListStaleFleetNodes(ctx, namespace)
}

// UUIDText renders a UUID in the canonical 8-4-4-4-12 form used in lock keys.
// An invalid UUID renders as an empty string.
func UUIDText(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
