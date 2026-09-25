package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/middleware"
)

// ManagedRuntimeEnroll exchanges a single-use enrollment secret for the daemon
// credential a managed (server-hosted) Aurora runtime is served with.
//
// The route sits outside DaemonAuth on purpose: the caller has no daemon token
// yet — that is exactly what this exchange mints. The credential is the raw
// mse_ secret in the Authorization header, and every piece of identity
// (workspace, runtime, node, daemon) comes from the hash lookup inside
// SandboxEnrollmentService.Consume. The endpoint decodes no request body, so a
// caller cannot name a workspace, runtime, provider, concurrency, or expiry.
//
// A spendable secret yields a workspace- and daemon-scoped mdt_ token plus the
// claude execution projection; the persisted runtime stays aurora_managed. Every
// failure that could distinguish unknown, expired, and replayed secrets
// collapses into one 401.
func (h *Handler) ManagedRuntimeEnroll(w http.ResponseWriter, r *http.Request) {
	if h.SandboxEnrollment == nil {
		writeFeatureDisabled(w, "aurora_sandbox_not_configured", "managed sandbox enrollment is not configured")
		return
	}

	consumed, runtime, err := h.SandboxEnrollment.Consume(r.Context(), middleware.BearerToken(r))
	if errors.Is(err, aurora.ErrInvalidManagedEnrollment) {
		writeError(w, http.StatusUnauthorized, "invalid managed enrollment")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enroll managed sandbox")
		return
	}

	// Identifiers only: neither the enrollment secret nor the minted daemon
	// token may reach a log line.
	slog.Info("managed sandbox enrolled",
		"workspace_id", uuidToString(consumed.Identity.WorkspaceID),
		"runtime_id", uuidToString(consumed.Identity.RuntimeID),
		"node_id", uuidToString(consumed.Identity.NodeID),
		"daemon_id", consumed.Identity.DaemonID,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"workspace_id":            uuidToString(consumed.Identity.WorkspaceID),
		"daemon_id":               consumed.Identity.DaemonID,
		"runtime":                 runtimeToResponse(runtime),
		"execution_provider":      aurora.ManagedExecutionProvider,
		"max_concurrency":         aurora.ManagedMaxConcurrency,
		"daemon_token":            consumed.DaemonToken,
		"daemon_token_expires_at": consumed.DaemonTokenExpiresAt,
	})
}
