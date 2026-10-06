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
