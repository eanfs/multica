// Package claimguard is the database-side claim barrier for local Fleet
// runtimes. It runs inside the caller's claim transaction so the shared node
// lock and the claim commit as one unit.
package claimguard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/barrier"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrFleetNodeUnavailable is returned whenever persisted Fleet state does not
// prove the node may accept claims. Callers treat any non-nil error as "do not
// claim"; the concrete cause stays in the wrapped message for logs.
var ErrFleetNodeUnavailable = errors.New("local fleet node is not accepting claims")

// healthFreshness bounds how long a persisted health observation can justify a
// claim. It matches the coordinator's stale-node window.
const healthFreshness = 30 * time.Second

// Guard validates local Fleet claim eligibility for one node namespace.
type Guard struct {
	namespace string
}

// New builds the guard for the Fleet namespace this API serves.
func New(namespace string) *Guard {
	return &Guard{namespace: namespace}
}

// CheckClaim must run inside the transaction that performs the claim. Order is
// deliberate: take the shared node lock BEFORE reading persisted state, so a
// maintenance operation holding the exclusive lock either completes before this
// check or waits for the claim to commit. Reading first would allow a stop
// request between the read and the claim (health checks alone cannot close that
// window).
//
// Ordinary runtimes skip every Fleet query. A runtime explicitly marked
// local_fleet must prove its barrier: a missing daemon id fails closed, because
// an incomplete fleet row must never be treated as an ordinary runtime.
func (g *Guard) CheckClaim(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID) error {
	rt, err := q.GetAgentRuntime(ctx, runtimeID)
	if err != nil {
		return fmt.Errorf("load runtime: %w", err)
	}
	if rt.ManagedBy != "local_fleet" {
		return nil
	}
	if !rt.DaemonID.Valid || rt.DaemonID.String == "" {
		return fmt.Errorf("%w: runtime has no daemon id", ErrFleetNodeUnavailable)
	}

	daemonID := rt.DaemonID.String
	if err := q.AcquireFleetNodeSharedLock(ctx, barrier.Key(g.namespace, daemonID)); err != nil {
		return fmt.Errorf("fleet claim lock: %w", err)
	}

	node, err := q.GetFleetNodeByDaemonID(ctx, db.GetFleetNodeByDaemonIDParams{
		DaemonID:  daemonID,
		Namespace: g.namespace,
	})
	if err != nil {
		// Missing rows and transient DB errors both fail closed: an unreadable
		// barrier must never be treated as permission.
		return fmt.Errorf("%w: load fleet node: %v", ErrFleetNodeUnavailable, err)
	}
	if node.TargetState != "running" || !node.DaemonReady {
		return ErrFleetNodeUnavailable
	}
	if !node.HealthCheckedAt.Valid || node.HealthCheckedAt.Time.IsZero() ||
		time.Since(node.HealthCheckedAt.Time) > healthFreshness {
		return ErrFleetNodeUnavailable
	}

	active, err := q.GetActiveFleetNodeCredentialCount(ctx, node.ID)
	if err != nil {
		return fmt.Errorf("count fleet node credentials: %w", err)
	}
	if active == 0 {
		return ErrFleetNodeUnavailable
	}
	return nil
}
