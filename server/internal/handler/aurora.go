package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/multica-ai/multica/server/internal/aurora"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// maxAuroraGenerationBodyBytes caps the request body for generation creation.
// A content prompt is small; the cap keeps an unbounded write from bloating
// the TEXT column.
const maxAuroraGenerationBodyBytes = 256 * 1024

// ListAuroraSkills returns the full catalog, including unavailable phase-2 skills.
// The router requires authentication and workspace membership.
func (h *Handler) ListAuroraSkills(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"skills": aurora.Catalog()})
}

// AuroraGenerationResponse is the consumer-facing shape of a generation row.
type AuroraGenerationResponse struct {
	ID              string `json:"id"`
	SkillID         string `json:"skillId"`
	Prompt          string `json:"prompt"`
	Status          string `json:"status"`
	CreditsReserved int64  `json:"creditsReserved"`
}

// CreateAuroraGeneration creates a queued generation row for the current user
// in the resolved workspace. Execution is wired in Plan 3; credits reservation
// lands in Plan 2. The router guards this route with RequireHumanActor and
// RequireWorkspaceMember: machine credentials cannot create generations, and
// workspace membership is re-validated on every request.
func (h *Handler) CreateAuroraGeneration(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, userID, "user_id")
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAuroraGenerationBodyBytes)
	var req struct {
		SkillID string `json:"skillId"`
		Prompt  string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SkillID == "" || strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "skillId and prompt are required")
		return
	}
	entry, ok := aurora.Lookup(req.SkillID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown skill")
		return
	}
	if !entry.Available {
		writeError(w, http.StatusBadRequest, "skill not available")
		return
	}

	row, err := h.Queries.CreateAuroraGeneration(r.Context(), db.CreateAuroraGenerationParams{
		WorkspaceID: workspaceID,
		UserID:      userUUID,
		SkillID:     req.SkillID,
		Prompt:      req.Prompt,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create generation")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"generation": AuroraGenerationResponse{
		ID:              uuidToString(row.ID),
		SkillID:         row.SkillID,
		Prompt:          row.Prompt,
		Status:          row.Status,
		CreditsReserved: row.CreditsReserved,
	}})
}
