package cloudruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/multica-ai/multica/server/internal/fleet"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
	"net/http"
)

func privateOperationError(resp *Response) error {
	if resp == nil {
		return model.ErrUnavailable
	}
	var body struct {
		Code string `json:"error_code"`
	}
	_ = json.Unmarshal(resp.Body, &body)
	switch {
	case resp.StatusCode == 409 && body.Code == "node_namespace_conflict":
		return model.ErrNodeNamespaceConflict
	case resp.StatusCode == 409 && body.Code == "busy":
		return model.ErrBusy
	case resp.StatusCode == 409 && body.Code == "unknown_health":
		return model.ErrUnknownHealth
	case resp.StatusCode == 409:
		return model.ErrConflict
	case resp.StatusCode == 403 || resp.StatusCode == 401:
		return model.ErrForbidden
	case resp.StatusCode == 400:
		return model.ErrInvalidRequest
	default:
		return fmt.Errorf("%w: private response status %d", model.ErrUnavailable, resp.StatusCode)
	}
}

// AuroraWorkspaceNodeRequest is the transport identity of one Aurora
// workspace-node intent. EnrollmentToken travels in the AuroraEnrollmentHeader,
// never in the JSON body, and the strict body therefore carries identity only.
type AuroraWorkspaceNodeRequest struct {
	NodeID          string
	WorkspaceID     string
	RuntimeID       string
	DaemonID        string
	EnrollmentToken string
	ImageDigest     string
	Name            string
	Spec            string
	IdempotencyKey  string
}

// EnsureAuroraWorkspaceNode provisions (or replays) one Aurora workspace node
// through the Fleet internal route. The response is the public NodeDTO.
func (c *Client) EnsureAuroraWorkspaceNode(ctx context.Context, owner string, req AuroraWorkspaceNodeRequest) (fleet.NodeDTO, error) {
	if _, e := util.ParseUUID(req.NodeID); e != nil {
		return fleet.NodeDTO{}, model.ErrInvalidRequest
	}
	body, e := json.Marshal(struct {
		WorkspaceID    string `json:"workspace_id"`
		RuntimeID      string `json:"runtime_id"`
		DaemonID       string `json:"daemon_id"`
		ImageDigest    string `json:"image_digest"`
		Name           string `json:"name"`
		Spec           string `json:"spec"`
		IdempotencyKey string `json:"idempotency_key"`
	}{req.WorkspaceID, req.RuntimeID, req.DaemonID, req.ImageDigest, req.Name, req.Spec, req.IdempotencyKey})
	if e != nil {
		return fleet.NodeDTO{}, e
	}
	headers := http.Header{}
	headers.Set(fleet.AuroraEnrollmentHeader, req.EnrollmentToken)
	resp, e := c.Do(ctx, Request{Method: http.MethodPut, Path: "/internal/v1/workspace-nodes/" + req.NodeID, UserID: owner, Body: body, Headers: headers, Op: "provision"})
	if e != nil {
		return fleet.NodeDTO{}, e
	}
	if resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fleet.NodeDTO{}, privateOperationError(resp)
	}
	var node fleet.NodeDTO
	if e = json.Unmarshal(resp.Body, &node); e != nil {
		return fleet.NodeDTO{}, e
	}
	if node.ID != req.NodeID || node.OwnerID != owner {
		return fleet.NodeDTO{}, model.ErrConflict
	}
	return node, nil
}

// DeleteAuroraWorkspaceNode records one destroy intent. The route is idempotent,
// so a node the Fleet no longer knows is success; a still-unknown 404/403 is
// reported as ErrNodeNotFound for callers that treat cleanup as repeatable.
func (c *Client) DeleteAuroraWorkspaceNode(ctx context.Context, owner, nodeID string) error {
	if _, e := util.ParseUUID(nodeID); e != nil {
		return model.ErrInvalidRequest
	}
	resp, e := c.Do(ctx, Request{Method: http.MethodDelete, Path: "/internal/v1/workspace-nodes/" + nodeID, UserID: owner, Op: "terminate"})
	if e != nil {
		return e
	}
	if resp == nil {
		return model.ErrUnavailable
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return ErrNodeNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return privateOperationError(resp)
	}
	return nil
}

func operationRequest(ref model.OperationRef) fleet.OperationRequestDTO {
	return fleet.OperationRequestDTO{Namespace: ref.Namespace, NodeID: util.UUIDToString(ref.NodeID), OperationID: util.UUIDToString(ref.OperationID), Generation: ref.Generation, Action: ref.Action}
}
func (c *Client) DiagnoseNode(ctx context.Context, owner string, ref model.OperationRef) (model.Observation, error) {
	request := operationRequest(ref)
	if _, e := request.OperationRef(); e != nil {
		return model.Observation{}, e
	}
	body, e := json.Marshal(request)
	if e != nil {
		return model.Observation{}, e
	}
	resp, e := c.Do(ctx, Request{Method: http.MethodPost, Path: "/internal/v1/nodes/diagnose", UserID: owner, Body: body})
	if e != nil {
		return model.Observation{}, e
	}
	if resp == nil || resp.StatusCode != 200 {
		return model.Observation{}, privateOperationError(resp)
	}
	var dto fleet.DiagnosticResponseDTO
	if e = json.Unmarshal(resp.Body, &dto); e != nil {
		return model.Observation{}, e
	}
	if dto.Request != request {
		return model.Observation{}, model.ErrConflict
	}
	if _, e = dto.Request.OperationRef(); e != nil {
		return model.Observation{}, e
	}
	return dto.Observation, nil
}
func (c *Client) ReviewOperation(ctx context.Context, owner string, ref model.OperationRef) (model.Operation, error) {
	request := operationRequest(ref)
	if _, e := request.OperationRef(); e != nil {
		return model.Operation{}, e
	}
	body, e := json.Marshal(request)
	if e != nil {
		return model.Operation{}, e
	}
	resp, e := c.Do(ctx, Request{Method: http.MethodPost, Path: "/internal/local-fleet/operations/review", UserID: owner, Body: body})
	if e != nil {
		return model.Operation{}, e
	}
	if resp == nil || resp.StatusCode != 200 {
		return model.Operation{}, privateOperationError(resp)
	}
	var dto fleet.OperationReviewResponseDTO
	if e = json.Unmarshal(resp.Body, &dto); e != nil {
		return model.Operation{}, e
	}
	got, e := dto.OperationRef()
	if e != nil {
		return model.Operation{}, e
	}
	if got != ref || dto.OwnerID != owner {
		return model.Operation{}, model.ErrConflict
	}
	ownerUUID, e := util.ParseUUID(dto.OwnerID)
	if e != nil {
		return model.Operation{}, e
	}
	return model.Operation{ID: got.OperationID, NodeID: got.NodeID, OwnerID: ownerUUID, Generation: got.Generation, Action: got.Action, Phase: dto.Phase, Approved: dto.Approved}, nil
}
