package fleetguard

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"time"
)

type Maintainer struct {
	Repo     *store.Store
	Diagnose func(context.Context, model.Node, model.OperationRef) (model.Observation, error)
}

func BusyObservation(o model.Observation) bool {
	return !o.ReportStatsKnown || o.ActiveRuns > 0 || o.PendingReports > 0 || o.FailedReports > 0
}
func (m *Maintainer) Request(ctx context.Context, owner, node pgtype.UUID, action model.Action, key string) (model.Operation, error) {
	if m == nil || m.Repo == nil {
		return model.Operation{}, model.ErrUnavailable
	}
	if action == model.Start {
		return m.Repo.CreateStartIntent(ctx, owner, node, key)
	}
	op, e := m.Repo.PrepareMaintenance(ctx, owner, node, action, key)
	if e != nil || op.Approved || op.Phase != "preparing" {
		if e == nil && !op.Approved {
			e = model.ErrConflict
		}
		return op, e
	}
	n, e := m.Repo.GetNode(ctx, owner, node)
	if e != nil {
		return op, e
	}
	return m.Review(ctx, owner, model.OperationRef{Namespace: n.Namespace, NodeID: node, OperationID: op.ID, Generation: op.Generation, Action: op.Action})
}
func (m *Maintainer) Review(ctx context.Context, owner pgtype.UUID, ref model.OperationRef) (model.Operation, error) {
	if m == nil || m.Repo == nil {
		return model.Operation{}, model.ErrUnavailable
	}
	n, op, e := m.Repo.MaintenanceSnapshot(ctx, owner, ref)
	if e != nil || op.Approved {
		return op, e
	}
	if m.Diagnose == nil {
		return op, model.ErrUnknownHealth
	}
	diagnosticCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	obs, e := m.Diagnose(diagnosticCtx, n, ref)
	deadline := diagnosticCtx.Err()
	cancel()
	if e != nil || deadline != nil {
		return op, model.ErrUnknownHealth
	}
	approvalErr := m.Repo.ApproveMaintenance(ctx, owner, ref, n, obs)
	if approvalErr != nil && !errors.Is(approvalErr, model.ErrBusy) {
		return op, approvalErr
	}
	updated, readErr := m.Repo.GetOperation(ctx, owner, op.ID)
	if readErr != nil {
		if approvalErr != nil {
			return op, approvalErr
		}
		return op, readErr
	}
	return updated, approvalErr
}
