package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The provider column the workspace's server-hosted Aurora runtime row carries.
// The aurora package's own constant is unexported, and this read must not import
// the sandbox lifecycle just to name a column value.
const auroraManagedRuntimeProvider = "aurora_managed"

// auroraExecutionNodeProvider is the only provider the local Docker Fleet
// serves Aurora sandbox nodes with.
const auroraExecutionNodeProvider = "docker"

// AuroraRuntimeNodeResponse is the public projection of the workspace's managed
// execution node. Identity and lifecycle state only: the enrollment secret,
// image digest, backend daemon id and raw failure reason stay server-side.
type AuroraRuntimeNodeResponse struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Ready       bool    `json:"ready"`
	Provider    string  `json:"provider"`
	ErrorCode   *string `json:"errorCode,omitempty"`
	OperationID *string `json:"operationId,omitempty"`
	CreatedAt   string  `json:"createdAt"`
}

// AuroraExecutionTargetResponse is the workspace's execution target as the
// runtime view consumes it. State is one of unconfigured, provisioning, online
// or failed.
type AuroraExecutionTargetResponse struct {
	WorkspaceID string                     `json:"workspaceId"`
	Node        *AuroraRuntimeNodeResponse `json:"node"`
	RuntimeID   *string                    `json:"runtimeId"`
	State       string                     `json:"state"`
}

// GetAuroraRuntime returns the workspace's managed execution node and managed
// runtime binding, read-only.
//
// The read never takes the node's lifecycle lock and never calls the Fleet: the
// runtime view is a projection, and a read that serialised against generation
// creation would turn opening a screen into a write-path dependency. It uses
// the non-locking GetAuroraSandboxNodeByWorkspace and GetAuroraManagedRuntime
// queries, so it cannot block or be blocked by Ensure.
func (h *Handler) GetAuroraRuntime(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}

	// No node row means the workspace has never provisioned a sandbox. That is
	// unconfigured, not failed: nothing has gone wrong yet.
	var node *db.AuroraSandboxNode
	row, err := h.Queries.GetAuroraSandboxNodeByWorkspace(r.Context(), workspaceID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to load execution node")
		return
	default:
		node = &row
	}

	// The managed runtime is seeded lazily on generation creation, so an
	// unseeded workspace legitimately has none.
	var runtimeID *string
	managed, err := h.Queries.GetAuroraManagedRuntime(r.Context(), db.GetAuroraManagedRuntimeParams{
		WorkspaceID: workspaceID,
		Provider:    auroraManagedRuntimeProvider,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to load managed runtime")
		return
	default:
		id := uuidToString(managed.ID)
		runtimeID = &id
	}

	writeJSON(w, http.StatusOK, AuroraExecutionTargetResponse{
		WorkspaceID: uuidToString(workspaceID),
		Node:        auroraRuntimeNodeResponse(node),
		RuntimeID:   runtimeID,
		State:       auroraExecutionState(node),
	})
}

// auroraRuntimeNodeResponse projects a node row into its consumer shape. It
// returns nil for a workspace with no row so the JSON node field is null.
func auroraRuntimeNodeResponse(node *db.AuroraSandboxNode) *AuroraRuntimeNodeResponse {
	if node == nil {
		return nil
	}
	// Prefer the Fleet node identity once the admission call has recorded it.
	// Before that, the row's own id is the identity the Fleet was asked for, so
	// it is the same public handle either way.
	id := uuidToString(node.ID)
	if node.BackendNodeID.Valid && node.BackendNodeID.String != "" {
		id = node.BackendNodeID.String
	}
	return &AuroraRuntimeNodeResponse{
		ID:          id,
		Status:      node.State,
		Ready:       node.State == "online",
		Provider:    auroraExecutionNodeProvider,
		ErrorCode:   auroraRuntimeErrorCode(*node),
		OperationID: nil, // the local node row does not persist the create operation id
		CreatedAt:   node.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
	}
}

// auroraRuntimeErrorCode maps a failed node's raw failure reason onto the public
// recovery vocabulary the runtime surfaces already speak. The raw reason names
// internal errors, and possibly endpoints, so it never leaves the server; an
// unrecognised failure collapses to "unavailable", which the client renders as
// generic recovery guidance.
func auroraRuntimeErrorCode(node db.AuroraSandboxNode) *string {
	if node.State != "failed" {
		return nil
	}
	code := "unavailable"
	if node.FailureReason.Valid {
		reason := strings.ToLower(node.FailureReason.String)
		switch {
		case strings.Contains(reason, "busy"):
			code = "busy"
		case strings.Contains(reason, "profile"),
			strings.Contains(reason, "credential"),
			strings.Contains(reason, "secret"):
			code = "profile_missing"
		case strings.Contains(reason, "not found"), strings.Contains(reason, "no such"):
			code = "instance_missing"
		}
	}
	return &code
}

// auroraExecutionState collapses the node row's lifecycle state into the four
// states the view renders. A draining node still serves work, so it reads as
// online; a stopped node reads as unconfigured because the next generation
// re-provisions it rather than leaving it dead.
func auroraExecutionState(node *db.AuroraSandboxNode) string {
	if node == nil {
		return "unconfigured"
	}
	switch node.State {
	case "starting":
		return "provisioning"
	case "online", "draining":
		return "online"
	case "failed":
		return "failed"
	default:
		return "unconfigured"
	}
}
