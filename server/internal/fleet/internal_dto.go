package fleet

import (
	"encoding/json"
	"time"

	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
)

type OperationRequestDTO struct {
	Namespace   string       `json:"namespace"`
	NodeID      string       `json:"node_id"`
	OperationID string       `json:"operation_id"`
	Generation  int64        `json:"generation"`
	Action      model.Action `json:"action"`
}
type DiagnosticResponseDTO struct {
	Request     OperationRequestDTO `json:"request"`
	Observation model.Observation   `json:"observation"`
}

// Private wire projection: domain observations and public node DTOs remain independent.
type observationWire struct {
	ContainerID      string    `json:"container_id"`
	Status           string    `json:"status"`
	DaemonID         string    `json:"daemon_id"`
	StartEpoch       string    `json:"start_epoch"`
	Ready            bool      `json:"ready"`
	RuntimeCount     int       `json:"runtime_count"`
	ActiveRuns       int       `json:"active_runs"`
	PendingReports   int       `json:"pending_reports"`
	FailedReports    int       `json:"failed_reports"`
	ReportStatsKnown bool      `json:"report_stats_known"`
	ObservedAt       time.Time `json:"observed_at"`
	Offline          bool      `json:"offline"`
	DataVolume       string    `json:"data_volume"`
	LayoutVersion    string    `json:"layout_version"`
}
type diagnosticWire struct {
	Request     OperationRequestDTO `json:"request"`
	Observation observationWire     `json:"observation"`
}

func (d DiagnosticResponseDTO) MarshalJSON() ([]byte, error) {
	o := d.Observation
	return json.Marshal(diagnosticWire{Request: d.Request, Observation: observationWire{ContainerID: o.ContainerID, Status: o.Status, DaemonID: o.DaemonID, StartEpoch: o.StartEpoch, Ready: o.Ready, RuntimeCount: o.RuntimeCount, ActiveRuns: o.ActiveRuns, PendingReports: o.PendingReports, FailedReports: o.FailedReports, ReportStatsKnown: o.ReportStatsKnown, ObservedAt: o.ObservedAt, Offline: o.Offline, DataVolume: o.DataVolume, LayoutVersion: o.LayoutVersion}})
}
func (d *DiagnosticResponseDTO) UnmarshalJSON(raw []byte) error {
	var wire diagnosticWire
	fields, e := model.DecodeStrictObject(raw, &wire)
	if e != nil {
		return e
	}
	if _, e = model.DecodeStrictObject(fields["request"], &wire.Request); e != nil {
		return e
	}
	if _, e = model.DecodeStrictObject(fields["observation"], &wire.Observation); e != nil {
		return e
	}
	o := wire.Observation
	d.Request = wire.Request
	d.Observation = model.Observation{ContainerID: o.ContainerID, Status: o.Status, DaemonID: o.DaemonID, StartEpoch: o.StartEpoch, Ready: o.Ready, RuntimeCount: o.RuntimeCount, ActiveRuns: o.ActiveRuns, PendingReports: o.PendingReports, FailedReports: o.FailedReports, ReportStatsKnown: o.ReportStatsKnown, ObservedAt: o.ObservedAt, Offline: o.Offline, DataVolume: o.DataVolume, LayoutVersion: o.LayoutVersion}
	return nil
}
func (d OperationRequestDTO) OperationRef() (model.OperationRef, error) {
	n, e := util.ParseUUID(d.NodeID)
	if e != nil || !n.Valid || n.Bytes == [16]byte{} {
		return model.OperationRef{}, model.ErrInvalidRequest
	}
	op, e := util.ParseUUID(d.OperationID)
	if e != nil || !op.Valid || op.Bytes == [16]byte{} || d.Namespace == "" || d.Generation < 1 {
		return model.OperationRef{}, model.ErrInvalidRequest
	}
	switch d.Action {
	case model.Create, model.Start, model.Stop, model.Reboot, model.Delete:
	default:
		return model.OperationRef{}, model.ErrInvalidRequest
	}
	return model.OperationRef{Namespace: d.Namespace, NodeID: n, OperationID: op, Generation: d.Generation, Action: d.Action}, nil
}
