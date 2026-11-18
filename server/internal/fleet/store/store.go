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

// ErrSchemaMissing reports that the shared database is reachable but the Fleet
// tables a process needs are not there. Readiness maps it to 503 instead of
// starting work against a half-migrated database.
var ErrSchemaMissing = errors.New("fleet schema missing")

// requiredTables are the tables every Fleet service path reads or writes. A
// database without them cannot serve a single node request, so readiness must
// not succeed on connectivity alone.
var requiredTables = []string{
	"fleet_node",
	"fleet_node_operation",
	"fleet_node_credential",
	"fleet_credential_profile",
}

// Store owns the PostgreSQL transaction boundaries for fleet state.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Queries returns the generated queries bound to the pool.
func (s *Store) Queries() *db.Queries { return db.New(s.pool) }

// Ping reports whether the shared PostgreSQL pool is reachable and the Fleet
// schema is present. Readiness probes use it so a Fleet process with a broken
// database, or one pointed at a database whose migrations never ran, never
// reports ready. Migrations are deliberately not started here.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("fleet store is not configured")
	}
	if err := s.pool.Ping(ctx); err != nil {
		return err
	}
	for _, table := range requiredTables {
		var exists bool
		// to_regclass resolves through the connection's search_path and does
		// not raise when the relation is absent.
		if err := s.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
			return fmt.Errorf("check fleet schema: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: missing table %s", ErrSchemaMissing, table)
		}
	}
	return nil
}

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
