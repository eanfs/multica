package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/cloudruntime"
	"github.com/multica-ai/multica/server/internal/fleet"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/fleetguard"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"io"
	"net/http"
	"time"
)

type fleetDiagnoser interface {
	DiagnoseNode(context.Context, string, model.OperationRef) (model.Observation, error)
}

func (h *Handler) localMaintainer(ctx context.Context, owner, node pgtype.UUID, namespace string) (*fleetguard.Maintainer, error) {
	pool, ok := h.TxStarter.(*pgxpool.Pool)
	if !ok || pool == nil {
		return nil, model.ErrUnavailable
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	identity, e := h.Queries.GetFleetNodeIdentity(c, db.GetFleetNodeIdentityParams{NodeID: node, OwnerID: owner})
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, model.ErrForbidden
	}
	if e != nil {
		return nil, e
	}
	if identity.OwnerID != owner || namespace != "" && namespace != identity.Namespace {
		return nil, model.ErrForbidden
	}
	d, ok := h.CloudRuntime.(fleetDiagnoser)
	if !ok {
		return nil, model.ErrUnavailable
	}
	repo := store.New(pool, identity.Namespace)
	return &fleetguard.Maintainer{Repo: repo, Diagnose: func(c context.Context, n model.Node, ref model.OperationRef) (model.Observation, error) {
		return d.DiagnoseNode(c, util.UUIDToString(n.OwnerID), ref)
	}}, nil
}
func (h *Handler) requestLocalOperation(w http.ResponseWriter, r *http.Request, userID string, body []byte, action model.Action, method, path string) {
	var in struct {
		InstanceID string `json:"instance_id"`
	}
	if _, e := model.DecodeStrictObject(body, &in); e != nil || in.InstanceID == "" {
		writeError(w, 400, "local lifecycle accepts only instance_id")
		return
	}
	node, ok := parseUUIDOrBadRequest(w, in.InstanceID, "instance_id")
	if !ok {
		return
	}
	owner, ok := parseUUIDOrBadRequest(w, userID, "owner_id")
	if !ok {
		return
	}
	m, e := h.localMaintainer(r.Context(), owner, node, "")
	if e != nil {
		writeLocalOperationError(w, e)
		return
	}
	op, e := m.Request(r.Context(), owner, node, action, r.Header.Get("Idempotency-Key"))
	if e != nil {
		writeLocalOperationError(w, e)
		return
	}
	n, e := m.Repo.GetNode(r.Context(), owner, node)
	if e != nil {
		writeLocalOperationError(w, e)
		return
	}
	dto := fleet.OperationRequestDTO{Namespace: n.Namespace, NodeID: util.UUIDToString(op.NodeID), OperationID: util.UUIDToString(op.ID), Generation: op.Generation, Action: op.Action}
	payload, e := json.Marshal(dto)
	if e != nil {
		writeLocalOperationError(w, e)
		return
	}
	// Acceptance is a fixed-reference acknowledgment, never physical provider work.
	resp, e := h.CloudRuntime.Do(r.Context(), cloudruntime.Request{Method: method, Path: path, Body: payload, UserID: userID, RequestID: cloudRuntimeRequestID(r), Headers: http.Header{"Idempotency-Key": []string{r.Header.Get("Idempotency-Key")}}})
	if e != nil {
		writeCloudRuntimeError(w, r, e)
		return
	}
	writeCloudRuntimeResponse(w, resp)
}
func writeLocalOperationError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, model.ErrInvalidRequest):
		writeErrorCode(w, 400, "invalid_request", "invalid operation request")
	case errors.Is(e, model.ErrForbidden):
		writeErrorCode(w, 403, "forbidden", "operation forbidden")
	case errors.Is(e, model.ErrConflict):
		writeErrorCode(w, 409, "conflict", "operation conflicts with current state")
	case errors.Is(e, model.ErrBusy):
		writeErrorCode(w, 409, "busy", "node has active work or queued work that must be cancelled before deletion")
	case errors.Is(e, model.ErrUnknownHealth):
		writeErrorCode(w, 409, "unknown_health", "node diagnostic proof is unavailable; maintenance barrier retained")
	default:
		writeErrorCode(w, 503, "unavailable", "node lifecycle is unavailable")
	}
}

// Mounted outside browser/PAT middleware; only the copied private service key authorizes it.
func (h *Handler) LocalFleetOperationsHandler(secret []byte) http.Handler {
	key := append([]byte(nil), secret...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.LocalFleetURL == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/internal/local-fleet/operations/review" {
			http.NotFound(w, r)
			return
		}
		if len(key) < 32 {
			writeLocalOperationError(w, model.ErrUnavailable)
			return
		}
		values := r.Header.Values("X-Fleet-Service-Key")
		if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), key) != 1 {
			writeLocalOperationError(w, model.ErrForbidden)
			return
		}
		owners := r.Header.Values("X-User-ID")
		if len(owners) != 1 {
			writeLocalOperationError(w, model.ErrForbidden)
			return
		}
		owner, e := util.ParseUUID(owners[0])
		if e != nil || !owner.Valid || owner.Bytes == [16]byte{} {
			writeLocalOperationError(w, model.ErrForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxCloudRuntimeRequestBodySize)
		raw, e := io.ReadAll(r.Body)
		if e != nil {
			writeError(w, 400, "invalid operation request")
			return
		}
		var request fleet.OperationRequestDTO
		if _, e = model.DecodeStrictObject(raw, &request); e != nil {
			writeLocalOperationError(w, model.ErrInvalidRequest)
			return
		}
		ref, e := request.OperationRef()
		if e != nil {
			writeLocalOperationError(w, e)
			return
		}
		m, e := h.localMaintainer(r.Context(), owner, ref.NodeID, ref.Namespace)
		if e != nil {
			writeLocalOperationError(w, e)
			return
		}
		op, e := m.Review(r.Context(), owner, ref)
		if e != nil {
			writeLocalOperationError(w, e)
			return
		}
		n, e := m.Repo.GetNode(r.Context(), owner, op.NodeID)
		if e != nil {
			writeLocalOperationError(w, e)
			return
		}
		dto, e := fleet.NewReviewResponse(n, op)
		if e != nil {
			writeLocalOperationError(w, e)
			return
		}
		writeJSON(w, 200, dto)
	})
}
