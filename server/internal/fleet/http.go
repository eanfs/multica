package fleet

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	maxRequestBodyBytes = 1 << 20
	readyTimeout        = 5 * time.Second
)

// NewHTTP returns the Fleet HTTP handler. Every /api/v1 route requires the
// service secret; health endpoints stay open for local probes.
func NewHTTP(s *Service, secret string) http.Handler {
	router := chi.NewRouter()
	router.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	router.Get("/readyz", s.handleReadyz)

	router.Group(func(router chi.Router) {
		router.Use(serviceAuth(secret))
		router.Get("/api/v1/", s.handleServiceInfo)
		router.Get("/api/v1/nodes", s.handleListNodes)
		router.Post("/api/v1/nodes", s.handleCreateNode)
		router.Delete("/api/v1/nodes", s.handleOperate("delete"))
		router.Post("/api/v1/nodes/start", s.handleOperate("start"))
		router.Post("/api/v1/nodes/stop", s.handleOperate("stop"))
		router.Post("/api/v1/nodes/reboot", s.handleOperate("reboot"))
		router.Post("/api/v1/nodes/status", s.handleOperate("status"))
		router.Post("/api/v1/nodes/diagnose", s.handleDiagnose)
		router.Post("/api/v1/pat/verify", s.handleVerifyPAT)
	})
	return router
}

// serviceAuth compares the bearer token in constant time and only then trusts
// the X-User-ID header written by the API.
func serviceAuth(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !validServiceSecret(r.Header.Get("Authorization"), secret) {
				writeErrorJSON(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
				return
			}
			if r.Header.Get("X-User-ID") == "" && r.URL.Path != "/api/v1/pat/verify" {
				writeErrorJSON(w, http.StatusUnauthorized, "missing_user", "X-User-ID is required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func validServiceSecret(authorization, secret string) bool {
	const scheme = "Bearer "
	if secret == "" || !strings.HasPrefix(authorization, scheme) {
		return false
	}
	got := strings.TrimPrefix(authorization, scheme)
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

func (s *Service) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.store == nil || s.provider == nil {
		writeServiceError(w, ErrNotConfigured)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeErrorJSON(w, http.StatusServiceUnavailable, "store_unavailable", "database unavailable")
		return
	}
	if err := s.provider.Ping(ctx); err != nil {
		writeServiceError(w, fmt.Errorf("%w: %v", ErrDockerUnavailable, err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Service) handleServiceInfo(w http.ResponseWriter, _ *http.Request) {
	response := map[string]any{
		"provider":           "docker",
		"region":             "local",
		"operations":         []string{"create", "start", "stop", "reboot", "delete", "status", "diagnose"},
		"specs":              []Spec{},
		"persistent_storage": true,
		"disk_quota":         false,
	}
	if s != nil {
		if len(s.cfg.Specs) > 0 {
			response["specs"] = s.cfg.Specs
		}
		if s.cfg.MaxNodesPerOwner > 0 {
			response["max_nodes_per_owner"] = s.cfg.MaxNodesPerOwner
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Service) handleListNodes(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.store == nil {
		writeServiceError(w, ErrNotConfigured)
		return
	}
	limit, offset, err := pagination(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	nodes, err := s.ListNodes(r.Context(), r.Header.Get("X-User-ID"), limit, offset)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	responses := make([]nodeResponse, 0, len(nodes))
	for _, node := range nodes {
		responses = append(responses, s.nodeResponse(node))
	}
	writeJSON(w, http.StatusOK, responses)
}

func (s *Service) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	if s == nil {
		writeServiceError(w, ErrNotConfigured)
		return
	}
	var req CreateNodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeServiceError(w, err)
		return
	}
	node, err := s.CreateNode(r.Context(), r.Header.Get("X-User-ID"), req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.nodeResponse(node))
}

func (s *Service) handleOperate(operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			InstanceID     string `json:"instance_id"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			writeServiceError(w, err)
			return
		}
		node, err := s.Operate(r.Context(), r.Header.Get("X-User-ID"), req.InstanceID, operation, req.IdempotencyKey)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.nodeResponse(node))
	}
}

func (s *Service) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	if s == nil {
		writeServiceError(w, ErrNotConfigured)
		return
	}
	var req struct {
		InstanceID string `json:"instance_id"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeServiceError(w, err)
		return
	}
	node, err := s.findNode(r.Context(), r.Header.Get("X-User-ID"), req.InstanceID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	report, err := s.Diagnose(r.Context(), node)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                        report.Status,
		"daemon_ready":                  report.Status == "running",
		"agents":                        report.Agents,
		"pending_terminal_report_count": report.PendingTerminalReportCount,
	})
}

func (s *Service) handleVerifyPAT(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.store == nil {
		writeServiceError(w, ErrNotConfigured)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeServiceError(w, err)
		return
	}
	identity, found, err := s.VerifyPAT(r.Context(), req.Token)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if !found || !identity.Valid {
		writeJSON(w, http.StatusOK, patVerifyResponse{Valid: false})
		return
	}
	writeJSON(w, http.StatusOK, patVerifyResponse{
		Valid:            true,
		OwnerID:          identity.OwnerID,
		InstanceID:       identity.InstanceID,
		InstanceRecordID: identity.InstanceRecordID,
	})
}

type patVerifyResponse struct {
	Valid            bool   `json:"valid"`
	OwnerID          string `json:"owner_id,omitempty"`
	InstanceID       string `json:"instance_id,omitempty"`
	InstanceRecordID string `json:"instance_record_id,omitempty"`
}

// nodeResponse is the stable client contract. Fleet-specific progress lives in
// metadata so the existing runtime fields keep their meaning.
type nodeResponse struct {
	ID           string            `json:"id"`
	OwnerID      string            `json:"owner_id"`
	InstanceID   string            `json:"instance_id"`
	Region       string            `json:"region"`
	InstanceType string            `json:"instance_type"`
	ImageID      string            `json:"image_id"`
	SubnetID     string            `json:"subnet_id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	Tags         map[string]string `json:"tags"`
	Metadata     map[string]any    `json:"metadata"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Provider     string            `json:"provider,omitempty"`
	Error        string            `json:"error,omitempty"`
}

func (s *Service) nodeResponse(node db.FleetNode) nodeResponse {
	return nodeResponse{
		ID:           store.UUIDText(node.ID),
		OwnerID:      store.UUIDText(node.OwnerID),
		InstanceID:   node.ContainerID,
		Region:       "local",
		InstanceType: nodeSpecID(node.Spec),
		ImageID:      node.Image,
		SubnetID:     "",
		Name:         node.Name,
		Status:       s.displayStatus(node),
		Tags:         map[string]string{},
		Metadata: map[string]any{
			"provider":       "docker",
			"daemon_id":      node.DaemonID,
			"target_state":   node.TargetState,
			"observed_state": node.ObservedState,
			"daemon_ready":   node.DaemonReady,
			"generation":     node.Generation,
		},
		CreatedAt: timestamp(node.CreatedAt),
		UpdatedAt: timestamp(node.UpdatedAt),
		Provider:  "docker",
		Error:     node.Error,
	}
}

// displayStatus derives the user-facing state from persisted observations. A
// stale or missing health sample never reads as running.
func (s *Service) displayStatus(node db.FleetNode) string {
	return DeriveDisplayState(node.TargetState, Observed{
		// A container recorded as missing no longer counts as present even
		// though its id remains in the row.
		ContainerExists: node.ContainerID != "" && node.ObservedState != StateInstanceMissing,
		ContainerState:  observedContainerState(node.ObservedState),
		Healthy:         node.ObservedState == StateRunning,
		DaemonReady:     node.DaemonReady,
		HealthFresh:     node.HealthCheckedAt.Valid && s.clock().Sub(node.HealthCheckedAt.Time) < healthFreshWindow,
	})
}

func observedContainerState(observed string) string {
	switch observed {
	case StateRunning:
		return "running"
	case StateStopped, StateTerminated:
		return "exited"
	default:
		return ""
	}
}

func nodeSpecID(raw []byte) string {
	var spec struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return ""
	}
	return spec.ID
}

func timestamp(value pgtype.Timestamptz) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}

func pagination(r *http.Request) (int, int, error) {
	limit, offset := 0, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return 0, 0, fmt.Errorf("%w: limit must be a positive integer", ErrInvalidRequest)
		}
		limit = parsed
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return 0, 0, fmt.Errorf("%w: offset must be a non-negative integer", ErrInvalidRequest)
		}
		offset = parsed
	}
	return limit, offset, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dest any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	// Clients may not smuggle region, image, mount, or script fields through.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return fmt.Errorf("%w: request body is too large", ErrInvalidRequest)
		}
		return fmt.Errorf("%w: invalid request body", ErrInvalidRequest)
	}
	return nil
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, ErrNotFound):
		writeErrorJSON(w, http.StatusNotFound, "not_found", "node not found")
	case errors.Is(err, ErrInvalidRequest):
		writeErrorJSON(w, http.StatusBadRequest, "invalid_request", "invalid request")
	case errors.Is(err, ErrSpecNotAllowed):
		writeErrorJSON(w, http.StatusUnprocessableEntity, "spec_not_allowed", "spec not allowed")
	case errors.Is(err, ErrProfileMissing):
		writeErrorJSON(w, http.StatusUnprocessableEntity, "credential_profile_missing", "credential profile missing")
	case errors.Is(err, ErrProfileInvalid):
		writeErrorJSON(w, http.StatusUnprocessableEntity, "credential_profile_invalid", "credential profile invalid")
	case errors.Is(err, ErrNodeBusy):
		writeErrorJSON(w, http.StatusConflict, "node_busy", "node busy")
	case errors.Is(err, ErrConflict):
		writeErrorJSON(w, http.StatusConflict, "conflict", "operation conflict")
	case errors.Is(err, store.ErrLimitReached):
		writeErrorJSON(w, http.StatusConflict, "node_limit_reached", "node limit reached")
	case errors.Is(err, ErrDockerUnavailable):
		writeErrorJSON(w, http.StatusServiceUnavailable, "docker_unavailable", "docker unavailable")
	case errors.Is(err, ErrImageMissing):
		writeErrorJSON(w, http.StatusServiceUnavailable, "image_missing", "runtime image missing")
	case errors.Is(err, ErrStateUncertain):
		writeErrorJSON(w, http.StatusServiceUnavailable, "state_uncertain", "node state uncertain")
	case errors.Is(err, ErrNotConfigured):
		writeErrorJSON(w, http.StatusServiceUnavailable, "not_configured", "fleet not configured")
	default:
		writeErrorJSON(w, http.StatusInternalServerError, "internal", "fleet operation failed")
	}
}

func writeErrorJSON(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
