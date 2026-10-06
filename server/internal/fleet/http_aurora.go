package fleet

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
)

// AuroraEnrollmentHeader carries the single-use managed-enrollment secret on the
// Aurora node route. It is deliberately a header rather than a JSON field: the
// provision DTO never contains, logs or returns the secret.
const AuroraEnrollmentHeader = "X-Aurora-Enrollment-Token"

// auroraNodeRequestDTO is the exact public body of an Aurora workspace-node
// intent. There is no image, resource, profile, environment or secret field by
// construction; unknown JSON fields are rejected by the strict decoder.
type auroraNodeRequestDTO struct {
	WorkspaceID    string `json:"workspace_id"`
	RuntimeID      string `json:"runtime_id"`
	DaemonID       string `json:"daemon_id"`
	ImageDigest    string `json:"image_digest"`
	Name           string `json:"name"`
	Spec           string `json:"spec"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (d auroraNodeRequestDTO) request(token string) (model.AuroraNodeRequest, error) {
	var out model.AuroraNodeRequest
	for _, field := range []struct {
		raw string
		dst *pgtype.UUID
	}{{d.WorkspaceID, &out.WorkspaceID}, {d.RuntimeID, &out.RuntimeID}} {
		id, err := util.ParseUUID(field.raw)
		if err != nil || !id.Valid || id.Bytes == [16]byte{} {
			return model.AuroraNodeRequest{}, model.ErrInvalidRequest
		}
		*field.dst = id
	}
	daemon, err := util.ParseUUID(d.DaemonID)
	if err != nil || !daemon.Valid || daemon.Bytes == [16]byte{} {
		return model.AuroraNodeRequest{}, model.ErrInvalidRequest
	}
	out.DaemonID = util.UUIDToString(daemon)
	out.ImageDigest = d.ImageDigest
	out.Name = d.Name
	out.Spec = d.Spec
	out.IdempotencyKey = d.IdempotencyKey
	out.EnrollmentToken = token
	return out, nil
}

// registerAuroraRoutes exposes the workspace-node surface only when the
// administrator selected the Aurora profile; otherwise the routes do not exist.
func (s *Service) registerAuroraRoutes(router chi.Router) {
	if s.cfg.Aurora == nil {
		return
	}
	router.Route("/internal/v1/workspace-nodes", func(r chi.Router) {
		r.Put("/{nodeID}", s.putAuroraNode)
		r.Get("/{nodeID}", s.getAuroraNode)
		r.Delete("/{nodeID}", s.deleteAuroraNode)
	})
}

func auroraPathNodeID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := util.ParseUUID(chi.URLParam(r, "nodeID"))
	if err != nil || !id.Valid || id.Bytes == [16]byte{} {
		httpError(w, model.ErrInvalidRequest)
		return id, false
	}
	return id, true
}

func (s *Service) putAuroraNode(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := owner(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	nodeID, ok := auroraPathNodeID(w, r)
	if !ok {
		return
	}
	var dto auroraNodeRequestDTO
	if !decodeBody(w, r, &dto) {
		return
	}
	token := r.Header.Get(AuroraEnrollmentHeader)
	if !model.ValidEnrollmentToken(token) {
		httpError(w, model.ErrInvalidRequest)
		return
	}
	req, err := dto.request(token)
	if err != nil {
		httpError(w, err)
		return
	}
	node, op, err := s.ProvisionAuroraNode(r.Context(), ownerID, nodeID, req)
	if err != nil {
		httpError(w, err)
		return
	}
	out := publicNode(node)
	out.OperationID = util.UUIDToString(op.ID)
	jsonResponse(w, http.StatusAccepted, out)
}

func (s *Service) getAuroraNode(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := owner(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	nodeID, ok := auroraPathNodeID(w, r)
	if !ok {
		return
	}
	node, err := s.GetAuroraNode(r.Context(), ownerID, nodeID)
	if err != nil {
		httpError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, publicNode(node))
}

func (s *Service) deleteAuroraNode(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := owner(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	nodeID, ok := auroraPathNodeID(w, r)
	if !ok {
		return
	}
	if err := s.DeleteAuroraNode(r.Context(), ownerID, nodeID); err != nil {
		httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
