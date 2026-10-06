package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var errLocalFleetIdentityProtected = errors.New("managed runtime identity is protected")

// localFleetNode resolves scope from SQL, then rechecks the exact current credential.
// Fleet HTTP identity is necessary but never authorizes a caller-selected daemon.
func (h *Handler) localFleetNode(w http.ResponseWriter, r *http.Request) (*db.FleetNode, bool) {
	if h.cfg.LocalFleetURL == "" {
		return nil, true
	}
	identity, ok := middleware.CloudNodeIdentity(r.Context())
	if !ok {
		return nil, true
	}
	owner, err := util.ParseUUID(identity.OwnerID)
	if err != nil || !owner.Valid {
		writeError(w, 401, "invalid node identity")
		return nil, false
	}
	id, err := util.ParseUUID(identity.InstanceRecordID)
	if err != nil || !id.Valid {
		writeError(w, 401, "invalid node identity")
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	node, err := h.Queries.GetFleetNodeIdentity(ctx, db.GetFleetNodeIdentityParams{NodeID: id, OwnerID: owner})
	if err != nil {
		return nil, writeLocalIdentityError(w, err)
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	verified, err := h.Queries.VerifyFleetCredential(ctx, db.VerifyFleetCredentialParams{Namespace: node.Namespace, TokenHash: auth.HashToken(token)})
	if err != nil {
		return nil, writeLocalIdentityError(w, err)
	}
	if verified.ID != node.ID || verified.OwnerID != owner || verified.Namespace != node.Namespace || verified.ContainerID != identity.InstanceID {
		writeError(w, 403, "node identity mismatch")
		return nil, false
	}
	return &verified, true
}

func writeLocalIdentityError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 403, "node identity mismatch")
	} else {
		writeError(w, 503, "node identity unavailable")
	}
	return false
}

func (h *Handler) localFleetRegistration(w http.ResponseWriter, r *http.Request, daemonID string) (*db.FleetNode, bool) {
	node, ok := h.localFleetNode(w, r)
	if !ok {
		return nil, false
	}
	if node != nil {
		if util.UUIDToString(node.DaemonID) != daemonID {
			writeError(w, 403, "node daemon mismatch")
			return nil, false
		}
		return node, true
	}
	return nil, true
}

func localFleetMetadata(raw []byte, node *db.FleetNode) []byte {
	var metadata map[string]any
	_ = json.Unmarshal(raw, &metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	delete(metadata, "managed_by")
	delete(metadata, "fleet_node_id")
	if node != nil {
		metadata["managed_by"] = "local_fleet"
		metadata["fleet_node_id"] = util.UUIDToString(node.ID)
	}
	result, _ := json.Marshal(metadata)
	return result
}

func (h *Handler) requireLocalFleetRuntime(w http.ResponseWriter, r *http.Request, rt db.AgentRuntime) bool {
	node, ok := h.localFleetNode(w, r)
	if !ok {
		return false
	}
	var metadata map[string]any
	_ = json.Unmarshal(rt.Metadata, &metadata)
	managed := metadata["managed_by"] == "local_fleet"
	if node == nil {
		if managed {
			writeError(w, 403, "managed runtime requires node authentication")
			return false
		}
		return true
	}
	if !managed || metadata["fleet_node_id"] != util.UUIDToString(node.ID) || rt.OwnerID != node.OwnerID || !rt.DaemonID.Valid || rt.DaemonID.String != util.UUIDToString(node.DaemonID) {
		writeError(w, 403, "node runtime mismatch")
		return false
	}
	return true
}
