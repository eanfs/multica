package fleet

import (
	"encoding/json"
	"fmt"
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
	observationFields, e := model.DecodeStrictObject(fields["observation"], &wire.Observation)
	if e != nil {
		return e
	}
	for _, key := range []string{"container_id", "status", "daemon_id", "start_epoch", "ready", "runtime_count", "active_runs", "pending_reports", "failed_reports", "report_stats_known", "observed_at", "offline", "data_volume", "layout_version"} {
		if _, ok := observationFields[key]; !ok {
			return model.ErrInvalidRequest
		}
	}
	if wire.Observation.RuntimeCount < 0 || wire.Observation.ActiveRuns < 0 || wire.Observation.PendingReports < 0 || wire.Observation.FailedReports < 0 {
		return model.ErrInvalidRequest
	}
	o := wire.Observation
	d.Request = wire.Request
	d.Observation = model.Observation{ContainerID: o.ContainerID, Status: o.Status, DaemonID: o.DaemonID, StartEpoch: o.StartEpoch, Ready: o.Ready, RuntimeCount: o.RuntimeCount, ActiveRuns: o.ActiveRuns, PendingReports: o.PendingReports, FailedReports: o.FailedReports, ReportStatsKnown: o.ReportStatsKnown, ObservedAt: o.ObservedAt, Offline: o.Offline, DataVolume: o.DataVolume, LayoutVersion: o.LayoutVersion}
	return nil
}

// OperationReviewResponseDTO is the private SQL-backed projection, never a raw domain operation.
type OperationReviewResponseDTO struct {
	Namespace   string       `json:"namespace"`
	OwnerID     string       `json:"owner_id"`
	NodeID      string       `json:"node_id"`
	OperationID string       `json:"operation_id"`
	Generation  int64        `json:"generation"`
	Action      model.Action `json:"action"`
	Phase       string       `json:"phase"`
	Approved    bool         `json:"approved"`
}

func NewReviewResponse(n model.Node, o model.Operation) (OperationReviewResponseDTO, error) {
	d := OperationReviewResponseDTO{Namespace: n.Namespace, OwnerID: util.UUIDToString(o.OwnerID), NodeID: util.UUIDToString(o.NodeID), OperationID: util.UUIDToString(o.ID), Generation: o.Generation, Action: o.Action, Phase: o.Phase, Approved: o.Approved}
	if n.ID != o.NodeID || n.OwnerID != o.OwnerID || n.Generation != o.Generation {
		return d, model.ErrConflict
	}
	return d, d.Validate()
}
func (d OperationReviewResponseDTO) OperationRef() (model.OperationRef, error) {
	return (OperationRequestDTO{Namespace: d.Namespace, NodeID: d.NodeID, OperationID: d.OperationID, Generation: d.Generation, Action: d.Action}).OperationRef()
}
func (d OperationReviewResponseDTO) Validate() error {
	if _, e := d.OperationRef(); e != nil {
		return e
	}
	owner, e := util.ParseUUID(d.OwnerID)
	if e != nil || !owner.Valid || owner.Bytes == [16]byte{} {
		return model.ErrInvalidRequest
	}

	// Only phases actually emitted by the Task7 maintenance producer are valid here.
	switch d.Phase {
	case "queued":
		if !d.Approved {
			return model.ErrInvalidRequest
		}
	case "preparing", "failed":
		if d.Approved {
			return model.ErrInvalidRequest
		}
	default:
		return model.ErrInvalidRequest
	}
	return nil
}
func (d *OperationReviewResponseDTO) UnmarshalJSON(raw []byte) error {
	type wire OperationReviewResponseDTO
	var w wire
	fields, e := model.DecodeStrictObject(raw, &w)
	if e != nil {
		return e
	}
	for _, key := range []string{"namespace", "owner_id", "node_id", "operation_id", "generation", "action", "phase", "approved"} {
		if field, ok := fields[key]; !ok || string(field) == "null" {
			return model.ErrInvalidRequest
		}
	}
	next := OperationReviewResponseDTO(w)
	if e := next.Validate(); e != nil {
		return fmt.Errorf("invalid review response: %w", e)
	}
	*d = next
	return nil
}
func (d OperationReviewResponseDTO) MarshalJSON() ([]byte, error) {
	type wire OperationReviewResponseDTO
	if e := d.Validate(); e != nil {
		return nil, e
	}
	return json.Marshal(wire(d))
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
