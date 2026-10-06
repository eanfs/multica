package store

import (
	"context"

	"github.com/multica-ai/multica/server/internal/fleet/model"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CheckSchema is a read-only, row-independent PG17+ readiness probe, bounded including pool acquisition.
func (s *Store) CheckSchema(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return model.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	q := db.New(s.pool)
	supported, err := q.FleetSchemaVersion(ctx)
	if err != nil || !supported {
		return model.ErrUnavailable
	}
	if err = q.FleetSchemaProbe(ctx); err != nil {
		return model.ErrUnavailable
	}
	return nil
}
