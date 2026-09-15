package handler

import (
	"encoding/json"
	"net/http"

	"github.com/multica-ai/multica/server/internal/aurora"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

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
// lands in Plan 2.
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

	var req struct {
		SkillID string `json:"skillId"`
		Prompt  string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SkillID == "" || req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "skillId and prompt are required")
		return
	}
	if !aurora.Exists(req.SkillID) {
		writeError(w, http.StatusBadRequest, "unknown skill")
		return
	}
	entry, _ := aurora.Lookup(req.SkillID)
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
