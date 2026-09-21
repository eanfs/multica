package handler

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// managedRuntimeResponse is the response envelope for a managed (server-hosted)
// runtime registration. The id is what the sandbox daemon adds to its
// machine-level claim set so it can pick up the workspace's Aurora system-agent
// tasks (Plan 3 Task 3).
type managedRuntimeResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	RuntimeMode string `json:"runtime_mode"`
	Status      string `json:"status"`
}

// ManagedRuntimeRegister activates a workspace's server-hosted Aurora runtime on
// behalf of a sandbox daemon and returns the runtime id that daemon should claim.
//
// The route sits outside DaemonAuth on purpose: the credential is a shared,
// server-issued secret (AURORA_SANDBOX_TOKEN) the fleet controller injects into
// sandbox nodes, not a per-daemon token. The token proves the caller is a
// sandbox daemon — that is the whole "managed identity" — so the request needs no
// daemon id. The comparison is constant-time; a wrong or missing token is a 401.
//
// The runtime stays unbound (daemon_id NULL) so the claim path's NULL-daemon_id
// tolerance hands its tasks to whichever sandbox daemon is serving the
// workspace; the provider/runtime_mode the seed wrote already identify it as the
// managed runtime. Registration marks it hosted: it flips status online and
// refreshes last_seen_at, which is exactly what the claim admission's freshness
// gate requires (seeding leaves the row offline with no last_seen_at).
func (h *Handler) ManagedRuntimeRegister(w http.ResponseWriter, r *http.Request) {
	want := h.cfg.AuroraSandboxToken
	if want == "" {
		writeFeatureDisabled(w, "aurora_sandbox_not_configured", "managed runtime registration is not configured")
		return
	}
	presented := middleware.BearerToken(r)
	if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(want)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid sandbox token")
		return
	}

	var req struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	workspaceID, ok := parseUUIDOrBadRequest(w, strings.TrimSpace(req.WorkspaceID), "workspace_id")
	if !ok {
		return
	}

	rt, err := h.Queries.GetAuroraManagedRuntime(r.Context(), db.GetAuroraManagedRuntimeParams{
		WorkspaceID: workspaceID,
		Provider:    aurora.ManagedRuntimeProvider,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Seeding (EnsureSystemAgents) runs lazily on generation creation, so an
		// unseeded workspace has no managed runtime to serve yet. The sandbox
		// daemon retries; nothing is claimable until a generation exists anyway.
		writeError(w, http.StatusNotFound, "managed runtime not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load managed runtime")
		return
	}

	online, err := h.Queries.MarkAgentRuntimeOnline(r.Context(), rt.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register managed runtime")
		return
	}

	slog.Info("managed runtime registered",
		"workspace_id", uuidToString(workspaceID),
		"runtime_id", uuidToString(online.ID))

	writeJSON(w, http.StatusOK, map[string]any{
		"runtime": managedRuntimeResponse{
			ID:          uuidToString(online.ID),
			Name:        online.Name,
			Provider:    online.Provider,
			RuntimeMode: online.RuntimeMode,
			Status:      online.Status,
		},
	})
}
