package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
//
// This is also the artifact half of Plan safety: every reported artifact is
// screened here, before any row is written, and a rejected one fails the
// generation with the reservation refunded instead of being stored. Screening
// on the completion path rather than after the fact is what makes "no asset
// row" true rather than approximately true.
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

	// Screen the whole batch before writing any row. An artifact the moderator
	// rejects must not leave its siblings stored: the generation is about to be
	// failed, and a library that still shows output from it would contradict
	// the generation's own status.
	screened := make([]db.CreateAuroraAssetParams, 0, len(req.Artifacts))
	for _, a := range req.Artifacts {
		if a.MediaURL == "" {
			writeError(w, http.StatusBadRequest, "artifact missing media_url")
			return
		}
		kind := aurora.AssetKind(entry, a.Format)
		decision, err := h.Moderation.ScreenAsset(r.Context(), a.MediaURL, kind)
		if err != nil {
			// Fail closed: an artifact that could not be inspected is not
			// stored, and the task fails rather than completing with output
			// nobody screened (spec §10 red line).
			slog.Error("aurora asset moderation failed",
				"generation_id", uuidToString(gen.ID), "error", err)
			h.blockGenerationForModeration(r.Context(), gen, "moderation adapter error: "+err.Error())
			writeError(w, http.StatusInternalServerError, "content moderation unavailable")
			return
		}
		if !decision.Allowed {
			h.blockGenerationForModeration(r.Context(), gen, decision.Reason)
			writeError(w, http.StatusUnprocessableEntity, decision.Reason)
			return
		}
		screened = append(screened, db.CreateAuroraAssetParams{
			GenerationID: gen.ID,
			WorkspaceID:  gen.WorkspaceID,
			Kind:         kind,
			MediaUrl:     pgtype.Text{String: a.MediaURL, Valid: true},
			Format:       pgtype.Text{String: a.Format, Valid: a.Format != ""},
		})
	}

	for _, params := range screened {
		if _, err := h.Queries.CreateAuroraAsset(r.Context(), params); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to record artifact")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"recorded": len(req.Artifacts)})
}

// blockGenerationForModeration fails a generation whose artifact did not pass
// screening: the audit row, the refund, and the terminal status. The caller
// then answers the daemon with a non-2xx, which is what makes the task itself
// fail — the settlement that follows finds the generation already terminal and
// is a no-op, so the reservation is refunded exactly once.
//
// auditReason is the adapter's own account of the rejection and goes to the
// audit trail; the generation's error field gets the fixed "moderation blocked"
// so the client-facing status does not vary with the adapter's wording.
func (h *Handler) blockGenerationForModeration(ctx context.Context, gen db.AuroraGeneration, auditReason string) {
	h.recordModerationBlock(ctx, gen.ID, gen.WorkspaceID, aurora.ModerationScopeAsset, auditReason)
	h.failGenerationAndRefund(ctx, gen.UserID, gen.WorkspaceID, gen.ID, gen.CreditsReserved, "moderation blocked")
}
