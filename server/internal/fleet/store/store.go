// Package store persists Fleet state in the API's PostgreSQL database.
package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const databaseTimeout = 2 * time.Second

type Store struct {
	pool      *pgxpool.Pool
	namespace string
	maxNodes  int
}
type Option func(*Store)

// WithMaxNodes configures the per-owner limit used by subsequent intent producers.
func WithMaxNodes(limit int) Option {
	return func(s *Store) {
		if limit > 0 {
			s.maxNodes = limit
		}
	}
}
func New(pool *pgxpool.Pool, namespace string, opts ...Option) *Store {
	s := &Store{pool: pool, namespace: namespace, maxNodes: 2}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WithTx bounds database work. Callers acquire node, capacity, then runtime/agent/task
// locks in that order and must not perform HTTP or provider I/O in the callback.
func (s *Store) WithTx(ctx context.Context, fn func(*db.Queries) error) error {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), databaseTimeout)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	// SET LOCAL never leaks limits into another pool borrower. The overall context
	// bounds commit; the server-side limits also bound queries using a caller context.
	if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='2s'; SET LOCAL idle_in_transaction_session_timeout='2s'; SET LOCAL transaction_timeout='2s'"); err != nil {
		return err
	}
	if err = fn(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListNodes(ctx context.Context, ownerID pgtype.UUID, limit, offset int32) ([]model.Node, error) {
	if limit < 0 || offset < 0 {
		return nil, model.ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	rows, err := db.New(s.pool).ListFleetNodesByOwner(ctx, db.ListFleetNodesByOwnerParams{Namespace: s.namespace, OwnerID: ownerID, PageLimit: limit, PageOffset: offset})
	if err != nil {
		return nil, err
	}
	nodes := make([]model.Node, 0, len(rows))
	for _, row := range rows {
		nodes = append(nodes, nodeFromRow(row))
	}
	return nodes, nil
}

func (s *Store) GetNode(ctx context.Context, ownerID, nodeID pgtype.UUID) (model.Node, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	row, err := db.New(s.pool).GetFleetNode(ctx, db.GetFleetNodeParams{Namespace: s.namespace, OwnerID: ownerID, NodeID: nodeID})
	if err != nil {
		return model.Node{}, err
	}
	return nodeFromRow(row), nil
}

func (s *Store) GetOperation(ctx context.Context, ownerID, operationID pgtype.UUID) (model.Operation, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	row, err := db.New(s.pool).GetFleetOperation(ctx, db.GetFleetOperationParams{Namespace: s.namespace, OwnerID: ownerID, OperationID: operationID})
	if err != nil {
		return model.Operation{}, err
	}
	return model.Operation{
		ID:             row.ID,
		NodeID:         row.NodeID,
		OwnerID:        row.OwnerID,
		Action:         model.Action(row.Action),
		Phase:          row.Phase,
		IdempotencyKey: row.IdempotencyKey,
		RequestHash:    row.RequestHash,
		PriorDesired:   row.PriorDesired,
		Generation:     row.Generation,
		Approved:       row.Approved,
		Attempts:       int(row.Attempts),
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}, nil
}

func nodeFromRow(row db.FleetNode) model.Node {
	var health time.Time
	if row.HealthAt.Valid {
		health = row.HealthAt.Time
	}
	return model.Node{
		ID:             row.ID,
		OwnerID:        row.OwnerID,
		Namespace:      row.Namespace,
		ContainerID:    row.ContainerID,
		DaemonID:       util.UUIDToString(row.DaemonID),
		Name:           row.Name,
		Spec:           row.Spec,
		Image:          row.Image,
		ProfileRef:     row.ProfileRef,
		StartEpoch:     row.StartEpoch,
		DataVolume:     row.DataVolume,
		SecretsVolume:  row.SecretsVolume,
		ErrorCode:      row.ErrorCode,
		ErrorMessage:   row.ErrorMessage,
		Desired:        row.Desired,
		Status:         row.Status,
		Generation:     row.Generation,
		Ready:          row.Ready,
		HealthAt:       health,
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
		ActiveRuns:     int(row.ActiveRuns),
		PendingReports: int(row.PendingReports),
		FailedReports:  int(row.FailedReports),
		Maintenance:    row.Maintenance,
		Revoked:        row.Revoked,
	}
}
