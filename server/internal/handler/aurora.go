package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/storage"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// maxAuroraGenerationBodyBytes caps the request body for generation creation.
// A content prompt is small; the cap keeps an unbounded write from bloating
// the TEXT column.
const maxAuroraGenerationBodyBytes = 256 * 1024

// microCreditsPerCredit converts a catalog credit into the ledger's
// micro-credit unit (1 credit = 1e6 micro). Reservation and refund both move
// the amount derived with this same constant, so one write reverses the other
// exactly.
const microCreditsPerCredit = 1_000_000

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

// CreateAuroraGeneration validates the skill, lazily seeds the workspace's
// Aurora system agents, inserts a queued generation row, reserves its credits,
// and enqueues the execution task. The router guards this route with
// RequireHumanActor and RequireWorkspaceMember: machine credentials cannot
// create generations, and workspace membership is re-validated on every
// request.
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

	// Lazy seed so the enqueue below always has an execution carrier. owner_id
	// references "user", and the caller is a real member, so the caller's id
	// satisfies the FK (in the personal-space MVP it is the workspace owner,
	// mirroring the Mika system-agent precedent).
	if err := h.ensureAuroraSystemAgents(r.Context(), workspaceID, userUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to prepare workspace agents")
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

	amountMicro := int64(entry.Credits) * microCreditsPerCredit
	reference := uuidToString(row.ID)

	// Reserve before enqueue: a short wallet must never leave behind a queued
	// task for work no one was charged for.
	if err := h.Credit.Reserve(r.Context(), userUUID, workspaceID, amountMicro, reference); err != nil {
		if errors.Is(err, aurora.ErrInsufficientCredits) {
			h.markGenerationFailed(r.Context(), workspaceID, row.ID, "insufficient credits")
			writeError(w, http.StatusPaymentRequired, "insufficient credits")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to reserve credits")
		return
	}

	// Enqueue on the skill's system agent, then write the task id and reserved
	// amount back onto the row. Any failure after the reservation refunds it, so
	// the user is never charged for work that never started.
	agent, err := h.Queries.GetAgentBySystemKey(r.Context(), db.GetAgentBySystemKeyParams{
		WorkspaceID: workspaceID,
		SystemKey:   pgtype.Text{String: "aurora:" + entry.ID, Valid: true},
	})
	if err != nil {
		h.failGenerationAndRefund(r.Context(), userUUID, workspaceID, row.ID, amountMicro, "system agent unavailable")
		writeError(w, http.StatusInternalServerError, "failed to enqueue generation")
		return
	}

	task, err := h.TaskService.EnqueueQuickCreateTask(r.Context(), workspaceID, userUUID, agent.ID, pgtype.UUID{}, req.Prompt, "high", "", pgtype.UUID{}, pgtype.UUID{}, nil)
	if err != nil {
		h.failGenerationAndRefund(r.Context(), userUUID, workspaceID, row.ID, amountMicro, "enqueue failed")
		writeError(w, http.StatusInternalServerError, "failed to enqueue generation")
		return
	}

	updated, err := h.Queries.UpdateAuroraGenerationTask(r.Context(), db.UpdateAuroraGenerationTaskParams{
		ID:              row.ID,
		TaskID:          task.ID,
		CreditsReserved: amountMicro,
		WorkspaceID:     workspaceID,
	})
	if err != nil {
		h.failGenerationAndRefund(r.Context(), userUUID, workspaceID, row.ID, amountMicro, "failed to record task")
		writeError(w, http.StatusInternalServerError, "failed to record generation task")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"generation": AuroraGenerationResponse{
		ID:              uuidToString(updated.ID),
		SkillID:         updated.SkillID,
		Prompt:          updated.Prompt,
		Status:          updated.Status,
		CreditsReserved: updated.CreditsReserved,
	}})
}

// ensureAuroraSystemAgents serialises the idempotent system-agent seed behind a
// per-workspace advisory lock. The managed-runtime lookup-then-insert inside
// aurora.EnsureSystemAgents has no ON CONFLICT arbiter (its NULL daemon_id sits
// outside migration 121's partial unique index), so this lock — not the queries
// — is what makes two concurrent first seeds produce one runtime row.
func (h *Handler) ensureAuroraSystemAgents(ctx context.Context, workspaceID, ownerID pgtype.UUID) error {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "aurora:"+uuidToString(workspaceID)); err != nil {
		return err
	}
	if err := aurora.EnsureSystemAgents(ctx, h.Queries.WithTx(tx), workspaceID, ownerID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// markGenerationFailed moves a generation to the terminal failed state with a
// reason and no credits charged. Used when the request aborts before any
// credits changed hands (an insufficient wallet).
func (h *Handler) markGenerationFailed(ctx context.Context, workspaceID, generationID pgtype.UUID, reason string) {
	if _, err := h.Queries.UpdateAuroraGenerationTerminal(ctx, db.UpdateAuroraGenerationTerminalParams{
		ID:             generationID,
		Status:         "failed",
		Error:          pgtype.Text{String: reason, Valid: true},
		CreditsCharged: 0,
		WorkspaceID:    workspaceID,
	}); err != nil {
		slog.Warn("aurora generation mark-failed error", "generation_id", uuidToString(generationID), "error", err)
	}
}

// failGenerationAndRefund rolls a post-reservation failure back: refund the
// reserved micro-credits (idempotent via the generation-id reference) and mark
// the generation failed. Both writes are best-effort — the request path is
// already returning an error — but a failed refund is logged so a customer is
// never silently left charged for work that never started.
func (h *Handler) failGenerationAndRefund(ctx context.Context, userID, workspaceID, generationID pgtype.UUID, amountMicro int64, reason string) {
	if err := h.Credit.Refund(ctx, userID, workspaceID, amountMicro, uuidToString(generationID)); err != nil {
		slog.Warn("aurora generation refund failed", "generation_id", uuidToString(generationID), "error", err)
	}
	h.markGenerationFailed(ctx, workspaceID, generationID, reason)
}

// Aurora list paging bounds. The ledger and generation lists are bounded by
// what a screen can show, not by what one query can carry.
const (
	defaultAuroraListLimit = 50
	maxAuroraListLimit     = 200
)

// auroraBillingUser resolves the authenticated caller's UUID for the two
// account-scoped billing reads. These endpoints report the *caller's* wallet,
// so they deliberately do not consult X-Workspace-ID: a balance that changed
// with the selected workspace would be the wrong number in every workspace but
// one.
func auroraBillingUser(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return pgtype.UUID{}, false
	}
	return parseUUIDOrBadRequest(w, userID, "user_id")
}

// GetAuroraBillingBalance returns the caller's spendable balance in
// micro-credits (1 credit = 1e6 micro; 1 USD = 1000 credit). A user who has
// never transacted has no wallet row, which reads as 0 rather than an error —
// the field is always present so the client never has to distinguish "no
// balance" from "no answer".
func (h *Handler) GetAuroraBillingBalance(w http.ResponseWriter, r *http.Request) {
	userUUID, ok := auroraBillingUser(w, r)
	if !ok {
		return
	}
	balance, err := h.Credit.Balance(r.Context(), userUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load balance")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"availableMicro": balance})
}

// AuroraBillingTransactionResponse is one ledger row as the billing screen
// consumes it. amountMicro is signed (deduction negative), balanceAfterMicro is
// the wallet after that row, and reference names the row's subject — a
// generation id for reservations and refunds, or a grant key — so the UI can
// label it.
type AuroraBillingTransactionResponse struct {
	ID                string `json:"id"`
	Kind              string `json:"kind"`
	AmountMicro       int64  `json:"amountMicro"`
	BalanceAfterMicro int64  `json:"balanceAfterMicro"`
	Reference         string `json:"reference"`
	CreatedAt         string `json:"createdAt"`
}

// ListAuroraBillingTransactions returns the caller's ledger, newest first.
// `limit` is optional and junk-tolerant: a missing, unparseable, or
// out-of-range value falls back to the default instead of erroring, so a
// client bug cannot turn a billing screen into a 400.
func (h *Handler) ListAuroraBillingTransactions(w http.ResponseWriter, r *http.Request) {
	userUUID, ok := auroraBillingUser(w, r)
	if !ok {
		return
	}
	rows, err := h.Queries.ListCreditTransactions(r.Context(), db.ListCreditTransactionsParams{
		UserID: userUUID,
		Limit:  auroraListLimit(r.URL.Query().Get("limit")),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load transactions")
		return
	}
	// Non-nil even when empty: `null` here would reach the client's zod
	// `.default([])` as a present-but-null value, and the billing list would
	// render against it.
	transactions := make([]AuroraBillingTransactionResponse, 0, len(rows))
	for _, row := range rows {
		transactions = append(transactions, AuroraBillingTransactionResponse{
			ID:                uuidToString(row.ID),
			Kind:              row.Kind,
			AmountMicro:       row.AmountMicro,
			BalanceAfterMicro: row.BalanceAfterMicro,
			Reference:         row.Reference,
			CreatedAt:         row.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"transactions": transactions})
}

// auroraListLimit reads a ?limit query param, falling back to the default for
// anything missing, unparseable, or outside 1..maxAuroraListLimit.
func auroraListLimit(raw string) int32 {
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxAuroraListLimit {
			return int32(n)
		}
	}
	return defaultAuroraListLimit
}

// auroraListOffset reads a ?offset query param, falling back to 0 for anything
// unparseable or negative.
func auroraListOffset(raw string) int32 {
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			return int32(n)
		}
	}
	return 0
}

// AuroraGenerationSummaryResponse is the consumer-facing shape of a generation
// row in the list and detail endpoints. It extends the create response with the
// fields the progress screen reads: charged credits, an optional error, and the
// creation timestamp.
type AuroraGenerationSummaryResponse struct {
	ID              string  `json:"id"`
	SkillID         string  `json:"skillId"`
	Prompt          string  `json:"prompt"`
	Status          string  `json:"status"`
	CreditsReserved int64   `json:"creditsReserved"`
	CreditsCharged  int64   `json:"creditsCharged"`
	Error           *string `json:"error"`
	CreatedAt       string  `json:"createdAt"`
}

// AuroraAssetResponse is one content asset a generation produced. The library
// list and the generation detail endpoint emit the same shape, so one client
// schema parses either surface.
type AuroraAssetResponse struct {
	ID           string  `json:"id"`
	GenerationID string  `json:"generationId"`
	Kind         string  `json:"kind"`
	MediaURL     *string `json:"mediaUrl"`
	Format       *string `json:"format"`
	CreatedAt    string  `json:"createdAt"`
}

// AuroraGenerationDetailResponse wraps the summary with the generation's assets.
type AuroraGenerationDetailResponse struct {
	AuroraGenerationSummaryResponse
	Assets []AuroraAssetResponse `json:"assets"`
}

// assetResponse projects an asset row into its consumer shape.
func assetResponse(row db.AuroraAsset) AuroraAssetResponse {
	return AuroraAssetResponse{
		ID:           uuidToString(row.ID),
		GenerationID: uuidToString(row.GenerationID),
		Kind:         row.Kind,
		MediaURL:     textToPtr(row.MediaUrl),
		Format:       textToPtr(row.Format),
		CreatedAt:    row.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
	}
}

// generationSummary projects a generation row into its consumer shape. The
// status is the row's stored value; the detail endpoint replaces it with the
// task-derived status for in-flight generations.
func generationSummary(row db.AuroraGeneration) AuroraGenerationSummaryResponse {
	return AuroraGenerationSummaryResponse{
		ID:              uuidToString(row.ID),
		SkillID:         row.SkillID,
		Prompt:          row.Prompt,
		Status:          row.Status,
		CreditsReserved: row.CreditsReserved,
		CreditsCharged:  row.CreditsCharged,
		Error:           textToPtr(row.Error),
		CreatedAt:       row.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
	}
}

// ListAuroraGenerations returns the workspace's generations, newest first.
// `limit` and `offset` are optional and junk-tolerant so a client bug cannot
// turn the progress screen into a 400. Status here is the row's stored value —
// in-flight generations stay "queued" until the execution layer writes the
// terminal state back; live progress reads the detail endpoint, which derives
// the status from the enqueued task.
func (h *Handler) ListAuroraGenerations(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	rows, err := h.Queries.ListAuroraGenerations(r.Context(), db.ListAuroraGenerationsParams{
		WorkspaceID: workspaceID,
		Limit:       auroraListLimit(r.URL.Query().Get("limit")),
		Offset:      auroraListOffset(r.URL.Query().Get("offset")),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list generations")
		return
	}
	generations := make([]AuroraGenerationSummaryResponse, 0, len(rows))
	for _, row := range rows {
		generations = append(generations, generationSummary(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"generations": generations})
}

// GetAuroraGeneration returns one generation with its assets. A generation
// still on the queue has its status derived from the enqueued agent task so the
// progress screen reflects in-flight work without a separate status sync
// pipeline — the terminal state is written back to the row by the execution
// layer.
func (h *Handler) GetAuroraGeneration(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	generationID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "generation id")
	if !ok {
		return
	}

	row, err := h.Queries.GetAuroraGeneration(r.Context(), db.GetAuroraGenerationParams{
		ID:          generationID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "generation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load generation")
		return
	}

	summary := generationSummary(row)
	summary.Status = h.deriveGenerationStatus(r.Context(), row)

	assets, err := h.Queries.ListAuroraAssets(r.Context(), db.ListAuroraAssetsParams{
		GenerationID: generationID,
		WorkspaceID:  workspaceID,
		Limit:        maxAuroraListLimit,
		Offset:       0,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load assets")
		return
	}
	assetResponses := make([]AuroraAssetResponse, 0, len(assets))
	for _, a := range assets {
		assetResponses = append(assetResponses, assetResponse(a))
	}

	writeJSON(w, http.StatusOK, map[string]any{"generation": AuroraGenerationDetailResponse{
		AuroraGenerationSummaryResponse: summary,
		Assets:                          assetResponses,
	}})
}

// deriveGenerationStatus returns the generation's effective status. Terminal
// states are stored on the row; a queued row's status is derived from its
// enqueued agent task so the progress screen can show in-flight work. A missing
// task row or a transient read failure leaves the stored status untouched —
// derivation is best-effort and read-side only.
func (h *Handler) deriveGenerationStatus(ctx context.Context, row db.AuroraGeneration) string {
	if row.Status != "queued" || !row.TaskID.Valid {
		return row.Status
	}
	task, err := h.Queries.GetAgentTaskStatus(ctx, row.TaskID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("aurora generation status derive failed", "generation_id", uuidToString(row.ID), "error", err)
		}
		return row.Status
	}
	switch task.Status {
	case "completed":
		return "completed"
	case "failed", "cancelled":
		return "failed"
	case "queued":
		return "queued"
	default: // dispatched, running, waiting_local_directory, deferred
		return "running"
	}
}

// ---------------------------------------------------------------------------
// Asset library — list, download, delete (spec §9.1 #6)
// ---------------------------------------------------------------------------

// ListAuroraAssets returns the workspace's content assets, newest first,
// optionally narrowed to one generation. `limit` and `offset` are junk-tolerant
// like the generation list, but a malformed `generationId` is a 400: a bad page
// size still shows the right list, a bad filter would show the wrong one.
func (h *Handler) ListAuroraAssets(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	// Absent generationId leaves the filter unset, so the list spans the whole
	// workspace library; the workspace id is always bound, which is what keeps
	// another workspace's generation from widening the result.
	generationID := pgtype.UUID{}
	if raw := r.URL.Query().Get("generationId"); raw != "" {
		generationID, ok = parseUUIDOrBadRequest(w, raw, "generationId")
		if !ok {
			return
		}
	}

	rows, err := h.Queries.ListAuroraAssets(r.Context(), db.ListAuroraAssetsParams{
		GenerationID: generationID,
		WorkspaceID:  workspaceID,
		Limit:        auroraListLimit(r.URL.Query().Get("limit")),
		Offset:       auroraListOffset(r.URL.Query().Get("offset")),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list assets")
		return
	}
	// Non-nil even when empty, so the client's `.default([])` never sees a
	// present-but-null list.
	assets := make([]AuroraAssetResponse, 0, len(rows))
	for _, row := range rows {
		assets = append(assets, assetResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": assets})
}

// loadAuroraAsset resolves one asset by path id inside the caller's workspace.
// Workspace membership is validated by the route's middleware; the query
// re-scopes by workspace_id so another workspace's asset id resolves to the
// same 404 a nonexistent one does.
func (h *Handler) loadAuroraAsset(w http.ResponseWriter, r *http.Request) (db.AuroraAsset, bool) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return db.AuroraAsset{}, false
	}
	assetID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "asset id")
	if !ok {
		return db.AuroraAsset{}, false
	}
	asset, err := h.Queries.GetAuroraAsset(r.Context(), db.GetAuroraAssetParams{
		ID:          assetID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "failed to load asset")
			return db.AuroraAsset{}, false
		}
		writeError(w, http.StatusNotFound, "asset not found")
		return db.AuroraAsset{}, false
	}
	return asset, true
}

// DownloadAuroraAsset hands back one asset's file. It follows the attachment
// download contract: a CloudFront or presign deployment gets a short-lived
// signed URL to follow, and a deployment with no signable URL (local disk,
// private object host) has the object streamed through the API instead. The
// asset is resolved inside the caller's workspace first, so another
// workspace's asset id is a 404 rather than a redirect.
func (h *Handler) DownloadAuroraAsset(w http.ResponseWriter, r *http.Request) {
	asset, ok := h.loadAuroraAsset(w, r)
	if !ok {
		return
	}
	// media_url is the only pointer to the stored object. A generation
	// completes with assets that always carry one, but a row without it has
	// nothing to serve and nothing to guess at.
	if !asset.MediaUrl.Valid || asset.MediaUrl.String == "" {
		writeError(w, http.StatusNotFound, "asset has no file")
		return
	}
	if h.Storage == nil {
		writeFeatureDisabled(w, "storage_not_configured", "storage not configured")
		return
	}

	mediaURL := asset.MediaUrl.String
	key := h.Storage.KeyFromURL(mediaURL)
	switch h.resolveAttachmentDownloadMode(mediaURL) {
	case attachmentDownloadModeCloudFront:
		if h.CFSigner == nil {
			writeError(w, http.StatusInternalServerError, "cloudfront asset downloads are not configured")
			return
		}
		h.setAttachmentPreviewSecurityHeaders(w)
		http.Redirect(w, r, h.CFSigner.SignedURLWithContentDisposition(
			mediaURL,
			storage.AttachmentContentDisposition(auroraAssetFilename(key, asset.Format)),
			time.Now().Add(h.attachmentDownloadURLTTL()),
		), http.StatusFound)
	case attachmentDownloadModePresign:
		presigner, ok := h.Storage.(storage.DownloadPresigner)
		if !ok {
			writeError(w, http.StatusInternalServerError, "asset storage does not support presigned downloads")
			return
		}
		signedURL, err := presigner.PresignGetWithContentDisposition(
			r.Context(),
			key,
			h.attachmentDownloadURLTTL(),
			storage.AttachmentContentDisposition(auroraAssetFilename(key, asset.Format)),
		)
		if err != nil {
			slog.Error("failed to presign aurora asset download", "asset_id", uuidToString(asset.ID), "key", key, "error", err)
			writeError(w, http.StatusBadGateway, "failed to create download URL")
			return
		}
		h.setAttachmentPreviewSecurityHeaders(w)
		http.Redirect(w, r, signedURL, http.StatusFound)
	case attachmentDownloadModeProxy:
		h.proxyAuroraAssetDownload(w, r, asset, key)
	default:
		writeError(w, http.StatusInternalServerError, "invalid asset download mode")
	}
}

// proxyAuroraAssetDownload streams an asset through the API for deployments
// with no signable storage URL, the same fallback DownloadAttachment takes in
// that mode. aurora_asset stores no filename or content type of its own, so
// both are derived from the object key and the recorded format.
func (h *Handler) proxyAuroraAssetDownload(w http.ResponseWriter, r *http.Request, asset db.AuroraAsset, key string) {
	reader, err := h.Storage.GetReader(r.Context(), key)
	if err != nil {
		slog.Warn("aurora asset object missing", "asset_id", uuidToString(asset.ID), "key", key, "error", err)
		writeError(w, http.StatusNotFound, "asset file not found")
		return
	}
	defer reader.Close()

	filename := auroraAssetFilename(key, asset.Format)
	contentType := "application/octet-stream"
	if asset.Format.Valid && asset.Format.String != "" {
		if resolved := mime.TypeByExtension("." + strings.ToLower(asset.Format.String)); resolved != "" {
			contentType = resolved
		}
	}
	w.Header().Set("Content-Type", contentType)
	// The endpoint exists to save the file, so the disposition is forced even
	// for the media types a browser would otherwise render inline.
	w.Header().Set("Content-Disposition", storage.AttachmentContentDisposition(filename))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.setAttachmentPreviewSecurityHeaders(w)

	// A seekable backend (local disk) gets Range and resume handling from the
	// standard library; a forward-only one (object store body) streams whole.
	if seeker, ok := reader.(io.ReadSeeker); ok {
		http.ServeContent(w, r, filename, time.Time{}, seeker)
		return
	}
	if _, err := io.Copy(w, reader); err != nil {
		slog.Warn("aurora asset stream interrupted", "asset_id", uuidToString(asset.ID), "key", key, "error", err)
	}
}

// auroraAssetFilename names the file a download saves. The object key's
// basename is the best name available; the recorded format is the fallback for
// a key that carries none.
func auroraAssetFilename(key string, format pgtype.Text) string {
	if base := path.Base(key); base != "" && base != "." && base != "/" && base != ".." {
		return base
	}
	if format.Valid && format.String != "" {
		return "asset." + format.String
	}
	return "asset"
}

// DeleteAuroraAsset removes one asset from the workspace's library. The stored
// object goes first, then the row. There is no cascade between the two, and the
// failure orders are not symmetric: an object delete is idempotent, so a row
// that briefly outlives its object is repaired by simply retrying this request,
// while a row deleted before its object strands a file that nothing points at
// and nothing can find again. A storage failure therefore fails the request and
// leaves the row in place. Removing an asset the caller's workspace does not
// hold is a 404 either way, so the endpoint is no existence oracle for other
// workspaces' assets.
func (h *Handler) DeleteAuroraAsset(w http.ResponseWriter, r *http.Request) {
	asset, ok := h.loadAuroraAsset(w, r)
	if !ok {
		return
	}

	// No storage backend means no object to reclaim; the row still goes.
	if h.Storage != nil && asset.MediaUrl.Valid && asset.MediaUrl.String != "" {
		if err := h.Storage.DeleteObject(r.Context(), h.Storage.KeyFromURL(asset.MediaUrl.String)); err != nil {
			slog.Error("failed to delete aurora asset object", "asset_id", uuidToString(asset.ID), "error", err)
			writeError(w, http.StatusInternalServerError, "failed to delete asset file")
			return
		}
	}

	// A row already removed by a concurrent delete leaves the same end state
	// this request asked for, so it is not an error.
	if _, err := h.Queries.DeleteAuroraAsset(r.Context(), db.DeleteAuroraAssetParams{
		ID:          asset.ID,
		WorkspaceID: asset.WorkspaceID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete asset")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
