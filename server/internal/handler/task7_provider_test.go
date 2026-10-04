package handler

import (
	"context"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"time"
)

type task7DiagnosticProvider struct{}

func (task7DiagnosticProvider) CheckAvailability(context.Context) error { return nil }
func (task7DiagnosticProvider) Ensure(context.Context, model.Node, model.Bootstrap) (model.Observation, error) {
	panic("physical Ensure unauthorized")
}
func (task7DiagnosticProvider) Inspect(context.Context, model.Node) (model.Observation, error) {
	panic("physical Inspect unauthorized")
}
func (task7DiagnosticProvider) Apply(context.Context, model.Node, model.Action) (model.Observation, error) {
	panic("physical Apply unauthorized")
}
func (task7DiagnosticProvider) Delete(context.Context, model.Node, model.OperationRef) error {
	panic("physical Delete unauthorized")
}
func (task7DiagnosticProvider) Diagnose(_ context.Context, n model.Node, _ model.OperationRef) (model.Observation, error) {
	return model.Observation{ContainerID: n.ContainerID, DaemonID: n.DaemonID, StartEpoch: n.StartEpoch, Status: "running", Ready: true, ReportStatsKnown: true, ObservedAt: time.Now().UTC()}, nil
}
