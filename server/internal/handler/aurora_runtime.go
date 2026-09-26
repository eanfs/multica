package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

// touchManagedSandboxNode extends the managed sandbox node's activity window
// after an accepted heartbeat, which is what keeps the idle reaper from
// stopping a live node. The query ignores a mismatched daemon and a node that
// is not online or draining, so a heartbeat with no daemon identity is a
// harmless no-op.
//
// It is called from both transports — HTTP heartbeat and WebSocket ack — because
// a healthy WS connection suppresses the HTTP tick, and a node that is only
// touched at enrollment would look idle to the reaper.
//
// A failed touch is logged with identifiers and the error but does not fail the
// heartbeat: the runtime heartbeat already succeeded, and turning this
// bookkeeping write into a 500 would push the daemon into a retry storm without
// making the node any more alive.
func (h *Handler) touchManagedSandboxNode(ctx context.Context, workspaceID pgtype.UUID, daemonID string) {
	if !workspaceID.Valid || daemonID == "" {
		return
	}
	if err := h.Queries.TouchAuroraSandboxNode(ctx, db.TouchAuroraSandboxNodeParams{
		WorkspaceID: workspaceID,
		DaemonID:    daemonID,
	}); err != nil {
		slog.Warn("managed sandbox heartbeat touch failed",
			"workspace_id", uuidToString(workspaceID),
			"daemon_id", daemonID,
			"error", err,
		)
	}
}

var (
	// errManagedSandboxNodeNotFound means the authenticated daemon's workspace
	// has no sandbox node row.
	errManagedSandboxNodeNotFound = errors.New("managed sandbox node not found")
	// errManagedSandboxNodeOwnerMismatch means the node exists but is bound to
	// a different daemon than the one presenting the credential.
	errManagedSandboxNodeOwnerMismatch = errors.New("managed sandbox node owner mismatch")
)

// ManagedRuntimeShutdown releases a managed sandbox daemon's control-plane
// binding. It is the graceful counterpart to enrollment: the daemon presents
// its mdt_ credential, and the server retires everything that credential could
// still do.
//
// Workspace and daemon identity come from the DaemonAuth context, never from
// the request body, so a caller cannot name another node. An abrupt container
// death never reaches this endpoint; the fleet sweeper owns that case.
func (h *Handler) ManagedRuntimeShutdown(w http.ResponseWriter, r *http.Request) {
	workspaceID := middleware.DaemonWorkspaceIDFromContext(r.Context())
	daemonID := middleware.DaemonIDFromContext(r.Context())
	if workspaceID == "" || daemonID == "" {
		writeError(w, http.StatusUnauthorized, "managed shutdown requires a daemon credential")
		return
	}
	workspaceUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	hashes, err := h.shutdownManagedSandbox(r.Context(), workspaceUUID, daemonID)
	switch {
	case errors.Is(err, errManagedSandboxNodeNotFound):
		writeError(w, http.StatusNotFound, "managed sandbox node not found")
		return
	case errors.Is(err, errManagedSandboxNodeOwnerMismatch):
		writeError(w, http.StatusForbidden, "daemon does not own managed sandbox node")
		return
	case err != nil:
		slog.Error("managed sandbox shutdown failed", "workspace_id", workspaceID, "daemon_id", daemonID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to shut down managed sandbox")
		return
	}

	// Revoke the cached credentials immediately; without this the
	// DaemonTokenCache TTL would let a retired token keep authenticating until
	// it expired on its own.
	for _, hash := range hashes {
		h.DaemonTokenCache.Invalidate(r.Context(), hash)
	}
	slog.Info("managed sandbox shut down",
		"workspace_id", workspaceID,
		"daemon_id", daemonID,
		"revoked_tokens", len(hashes),
	)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// shutdownManagedSandbox performs the whole graceful release in one
// transaction: the bound runtime goes offline and loses its daemon binding, the
// node stops with its live enrollment/backend fields cleared but its audit
// timestamps kept, and every daemon token for the workspace/daemon is deleted.
// It returns the deleted token hashes so the caller can invalidate the cache
// only after the transaction commits.
func (h *Handler) shutdownManagedSandbox(ctx context.Context, workspaceID pgtype.UUID, daemonID string) ([]string, error) {
	if h.TxStarter == nil {
		return nil, errors.New("managed shutdown requires a transaction starter")
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)

	// Serialise with enrollment issuance and consumption for this workspace:
	// the same row lock makes the stop and the runtime release mutually
	// exclusive with a rotation.
	node, err := qtx.LockAuroraSandboxNodeByWorkspace(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errManagedSandboxNodeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock sandbox node: %w", err)
	}
	if node.DaemonID != daemonID {
		return nil, errManagedSandboxNodeOwnerMismatch
	}

	if err := qtx.ReleaseAuroraManagedRuntime(ctx, db.ReleaseAuroraManagedRuntimeParams{
		ID:          node.RuntimeID,
		WorkspaceID: node.WorkspaceID,
		DaemonID:    pgtype.Text{String: daemonID, Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("release managed runtime: %w", err)
	}

	if _, err := qtx.MarkAuroraSandboxNodeStopped(ctx, db.MarkAuroraSandboxNodeStoppedParams{
		WorkspaceID: workspaceID,
		DaemonID:    daemonID,
	}); err != nil {
		return nil, fmt.Errorf("stop sandbox node: %w", err)
	}

	hashes, err := qtx.DeleteDaemonTokensByWorkspaceAndDaemons(ctx, db.DeleteDaemonTokensByWorkspaceAndDaemonsParams{
		WorkspaceID: workspaceID,
		DaemonIds:   []string{daemonID},
	})
	if err != nil {
		return nil, fmt.Errorf("delete daemon tokens: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return hashes, nil
}
