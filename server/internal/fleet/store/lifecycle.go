package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"strings"
	"time"
)

const lifecycleLease = 5 * time.Second

// LifecycleClaim is private winner identity, never reconstructible from discovery.
type LifecycleClaim struct {
	snapshot            RecoverySnapshot
	claimedAt, deadline time.Time
}

func (c LifecycleClaim) Context(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(parent, c.deadline)
}
func (c LifecycleClaim) Snapshot() RecoverySnapshot { return c.snapshot }
func lifecycleState(s RecoverySnapshot) string {
	op, n := s.Operation, s.Node
	if (op.Action != model.Start && op.Action != model.Stop && op.Action != model.Reboot) || op.NonRetryable || op.Attempts >= 5 || n.Revoked || n.ContainerID == "" || n.Generation != op.Generation {
		return "denied"
	}
	if op.Action == model.Start {
		if n.Maintenance || n.Desired != "running" {
			return "denied"
		}
	} else if !op.Approved || !n.Maintenance || (op.Action == model.Stop && n.Desired != "stopped") || (op.Action == model.Reboot && n.Desired != "running") {
		return "denied"
	}
	if op.ActionClaimedAt.IsZero() {
		if op.Phase == "queued" || op.Phase == "prepared" {
			return "new"
		}
		return "denied"
	}
	if op.Phase != "applying" || op.ActionClaimedAt.After(s.SQLNow) {
		return "denied"
	}
	if s.SQLNow.Before(op.ActionClaimedAt.Add(lifecycleLease)) {
		return "live"
	}
	return "recovery"
}
func validLifecycleClaim(c LifecycleClaim, s RecoverySnapshot, local time.Time) bool {
	return !c.claimedAt.IsZero() && local.Before(c.deadline) && c.claimedAt.Equal(s.Operation.ActionClaimedAt) && sameRecoverySnapshot(c.snapshot, s) && lifecycleState(s) == "live"
}
func (s *Store) ClaimLifecycle(ctx context.Context, baseline RecoverySnapshot) (LifecycleClaim, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var out LifecycleClaim
	e := s.WithTx(ctx, func(q *db.Queries) error {
		snap, e := s.recoverySnapshot(ctx, q, baseline.Node.OwnerID, baseline.Ref())
		if e != nil {
			return e
		}
		if e = currentRecovery(snap); e != nil {
			return e
		}
		if !sameRecoverySnapshot(baseline, snap) || lifecycleState(snap) != "new" {
			return model.ErrConflict
		}
		n, op := snap.Node, snap.Operation
		row, e := q.FleetClaimLifecycle(ctx, db.FleetClaimLifecycleParams{Namespace: n.Namespace, OwnerID: n.OwnerID, NodeID: n.ID, OperationID: op.ID, Generation: op.Generation, Action: string(op.Action), Approved: op.Approved, ContainerID: n.ContainerID, StartEpoch: n.StartEpoch})
		if errors.Is(e, pgx.ErrNoRows) {
			return model.ErrConflict
		}
		if e != nil {
			return e
		}
		snap.Operation = operationFromRow(row)
		now, e := q.FleetRecoveryClock(ctx)
		if e != nil {
			return e
		}
		if !now.Valid || now.InfinityModifier != pgtype.Finite {
			return model.ErrUnavailable
		}
		snap.SQLNow = now.Time
		if lifecycleState(snap) != "live" {
			return model.ErrConflict
		}
		out = LifecycleClaim{snapshot: snap, claimedAt: snap.Operation.ActionClaimedAt, deadline: started.Add(snap.Operation.ActionClaimedAt.Add(lifecycleLease).Sub(now.Time))}
		return nil
	})
	return out, e
}
func (s *Store) currentLifecycle(ctx context.Context, q *db.Queries, c LifecycleClaim) (RecoverySnapshot, error) {
	snap, e := s.recoverySnapshot(ctx, q, c.snapshot.Node.OwnerID, c.snapshot.Ref())
	if e != nil {
		return snap, e
	}
	if !validLifecycleClaim(c, snap, time.Now()) {
		return snap, model.ErrConflict
	}
	return snap, nil
}
func (s *Store) CheckLifecycleClaim(ctx context.Context, c LifecycleClaim) (model.Node, error) {
	ctx, leaseCancel := c.Context(ctx)
	defer leaseCancel()
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var n model.Node
	e := s.WithTx(ctx, func(q *db.Queries) error { snap, e := s.currentLifecycle(ctx, q, c); n = snap.Node; return e })
	return n, e
}
func (s *Store) RecordLifecycleResult(ctx context.Context, c LifecycleClaim, o model.Observation) error {
	ctx, leaseCancel := c.Context(ctx)
	defer leaseCancel()
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	return s.WithTx(ctx, func(q *db.Queries) error {
		snap, e := s.currentLifecycle(ctx, q, c)
		if e != nil {
			return e
		}
		return s.persistOperationResult(ctx, q, snap, o, true)
	})
}

// Recovery must prove the checkpointed action, never merely a pre-existing healthy epoch.
func lifecycleObservation(s RecoverySnapshot, o model.Observation) error {
	op, n := s.Operation, s.Node
	if op.ActionClaimedAt.IsZero() || op.ActionClaimedAt.After(s.SQLNow) || op.Phase != "applying" || o.ObservedAt.Before(op.ActionClaimedAt) || o.Offline {
		return model.ErrUnknownHealth
	}
	if op.Action == model.Stop {
		if o.Ready || (o.Status != "stopped" && o.Status != "missing") {
			return model.ErrUnknownHealth
		}
		return nil
	}
	epoch, e := time.Parse(time.RFC3339Nano, o.StartEpoch)
	if e != nil || epoch.Before(op.ActionClaimedAt) || epoch.After(o.ObservedAt) || o.StartEpoch == op.ActionStartEpoch {
		return model.ErrUnknownHealth
	}
	if op.ActionStartEpoch != "" {
		old, e := time.Parse(time.RFC3339Nano, op.ActionStartEpoch)
		if e != nil || !epoch.After(old) {
			return model.ErrUnknownHealth
		}
	}
	if o.ContainerID != n.ContainerID || o.DaemonID != n.DaemonID {
		return model.ErrUnknownHealth
	}
	if o.Ready {
		claude := false
		for _, a := range o.Agents {
			claude = claude || strings.EqualFold(a, "claude")
		}
		if o.Status != "running" || !claude || o.RuntimeCount < 1 {
			return model.ErrUnknownHealth
		}
	}
	return nil
}
func (s *Store) persistOperationResult(ctx context.Context, q *db.Queries, snap RecoverySnapshot, o model.Observation, winner bool) error {
	var e error
	if e = currentRecovery(snap); e != nil {
		return e
	}
	if snap.bootstrapSuccess != nil && !validBootstrapSuccess(snap) {
		return model.ErrConflict
	}
	if snap.Operation.Action == model.Start || snap.Operation.Action == model.Stop || snap.Operation.Action == model.Reboot {
		if !winner && lifecycleState(snap) != "recovery" {
			return model.ErrConflict
		}
		if e := lifecycleObservation(snap, o); e != nil {
			return e
		}
	}
	n, op := snap.Node, snap.Operation
	// A late healthy observation cannot complete a create beyond its original initialization window.
	if op.Action == model.Create && !validBootstrapSuccess(snap) && !op.BootstrapClaimedAt.IsZero() && (!snap.SQLNow.Before(op.BootstrapClaimedAt.Add(bootstrapLease)) || op.BootstrapClaimedAt.After(snap.SQLNow)) {
		return model.ErrConflict
	}
	if op.Action == model.Delete || (maintenanceAction(op.Action) && !op.Approved) || (op.Action == model.Create && n.ContainerID == "") || o.Offline || o.ObservedAt.Before(op.CreatedAt) {
		return model.ErrConflict
	}
	check := n
	if (op.Action == model.Start || op.Action == model.Reboot) && o.StartEpoch != "" && o.StartEpoch != n.StartEpoch {
		next, e := time.Parse(time.RFC3339Nano, o.StartEpoch)
		if e != nil || next.After(o.ObservedAt) || next.Before(op.CreatedAt) {
			return model.ErrUnknownHealth
		}
		if n.StartEpoch != "" {
			old, e := time.Parse(time.RFC3339Nano, n.StartEpoch)
			if e != nil || !next.After(old) {
				return model.ErrUnknownHealth
			}
		}
		check.StartEpoch = ""
	}
	check.Maintenance = false
	if e = validateObservation(check, n.Generation, o, snap.SQLNow); e != nil {
		return e
	}
	if validBootstrapSuccess(snap) && !bootstrapHealthy(n, o, snap.SQLNow) {
		return model.ErrUnknownHealth
	}
	if e = persistObservation(ctx, q, n, o); e != nil {
		return e
	}
	complete := o.Status == "running" && o.Ready
	if op.Action == model.Stop {
		complete = o.Status == "stopped" || o.Status == "missing"
	}
	if !complete {
		return nil
	}
	epoch := n.StartEpoch
	if o.StartEpoch != "" {
		epoch = o.StartEpoch
	}
	var successReceipt []byte
	if validBootstrapSuccess(snap) {
		successReceipt, e = json.Marshal(snap.bootstrapSuccess)
		if e != nil {
			return model.ErrUnavailable
		}
	}
	count, e := q.FleetFinishLifecycleNode(ctx, db.FleetFinishLifecycleNodeParams{Namespace: s.namespace, OwnerID: n.OwnerID, NodeID: n.ID, Generation: n.Generation, Desired: n.Desired, Status: o.Status, StartEpoch: epoch, Ready: o.Ready})
	if e = affectedOne(count, e); e != nil {
		return e
	}
	count, e = q.FleetCompleteOperation(ctx, db.FleetCompleteOperationParams{SuccessReceipt: successReceipt, Namespace: s.namespace, OwnerID: n.OwnerID, NodeID: n.ID, OperationID: op.ID, Generation: op.Generation, Action: string(op.Action), Phase: op.Phase, Approved: op.Approved})
	return affectedOne(count, e)
}
