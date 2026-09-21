package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/aurora"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AuroraArtifactPayload is one content artifact the sandbox daemon produced and
// uploaded to storage for a completed generation. name is the original file
// name (kept on the wire for the daemon and future surfaces; aurora_asset has
// no name column, so it is not persisted). mediaURL is the uploaded object's
// URL; format is the file format (e.g. "png", "mp4", "pdf").
type AuroraArtifactPayload struct {
	Name     string `json:"name"`
	MediaURL string `json:"media_url"`
	Format   string `json:"format"`
}

// ReportTaskArtifacts records the artifacts a completed Aurora task produced.
// It is the out-of-band channel Plan 3 Task 4 introduces — the daemon uploads
// the workdir output to storage, then reports this structured list here (the
// same ReportTaskUsage-style daemon channel) before reporting the task
// complete. The daemon treats any non-2xx response as a report failure and
// fails the task, so a write error here must not be swallowed.
func (h *Handler) ReportTaskArtifacts(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	task, ok := h.requireDaemonTaskAccess(w, r, taskID)
	if !ok {
		return
	}

	var req struct {
		Artifacts []AuroraArtifactPayload `json:"artifacts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	gen, err := h.Queries.GetAuroraGenerationByTaskID(r.Context(), task.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not an aurora generation")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load generation")
		return
	}
	entry, ok := aurora.Lookup(gen.SkillID)
	if !ok {
		// A generation always references a catalog skill; a missing entry is a
		// data inconsistency. Fail rather than write assets with an empty kind.
		writeError(w, http.StatusInternalServerError, "unknown skill")
		return
	}

	for _, a := range req.Artifacts {
		if a.MediaURL == "" {
			writeError(w, http.StatusBadRequest, "artifact missing media_url")
			return
		}
		if _, err := h.Queries.CreateAuroraAsset(r.Context(), db.CreateAuroraAssetParams{
			GenerationID: gen.ID,
			WorkspaceID:  gen.WorkspaceID,
			Kind:         aurora.AssetKind(entry, a.Format),
			MediaUrl:     pgtype.Text{String: a.MediaURL, Valid: true},
			Format:       pgtype.Text{String: a.Format, Valid: a.Format != ""},
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to record artifact")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"recorded": len(req.Artifacts)})
}
