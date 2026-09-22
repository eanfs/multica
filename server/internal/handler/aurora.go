package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/aurora"
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

// Aurora transaction paging bounds. The ledger is the user's own history, so
// the page is bounded by what a billing screen can show, not by what one
// query can carry.
const (
	defaultAuroraTransactionLimit = 50
	maxAuroraTransactionLimit     = 200
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
		Limit:  auroraTransactionLimit(r.URL.Query().Get("limit")),
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

// auroraTransactionLimit reads the ?limit query param, falling back to the
// default for anything outside 1..maxAuroraTransactionLimit.
func auroraTransactionLimit(raw string) int32 {
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxAuroraTransactionLimit {
			return int32(n)
		}
	}
	return defaultAuroraTransactionLimit
}

// Aurora generation read paging bounds. The list is the workspace's own
// history, so the page is bounded by what a progress screen can show, not by
// what one query can carry.
const (
	defaultAuroraGenerationLimit = 50
	maxAuroraGenerationLimit     = 200
)

// auroraGenerationLimit reads the ?limit query param, falling back to the
// default for anything outside 1..maxAuroraGenerationLimit.
func auroraGenerationLimit(raw string) int32 {
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxAuroraGenerationLimit {
			return int32(n)
		}
	}
	return defaultAuroraGenerationLimit
}

// auroraGenerationOffset reads the ?offset query param, falling back to 0 for
// anything unparseable or negative.
func auroraGenerationOffset(raw string) int32 {
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

// AuroraGenerationAssetResponse is one content asset a generation produced.
type AuroraGenerationAssetResponse struct {
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
	Assets []AuroraGenerationAssetResponse `json:"assets"`
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
		Limit:       auroraGenerationLimit(r.URL.Query().Get("limit")),
		Offset:      auroraGenerationOffset(r.URL.Query().Get("offset")),
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
		Limit:        maxAuroraGenerationLimit,
		Offset:       0,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load assets")
		return
	}
	assetResponses := make([]AuroraGenerationAssetResponse, 0, len(assets))
	for _, a := range assets {
		assetResponses = append(assetResponses, AuroraGenerationAssetResponse{
			ID:           uuidToString(a.ID),
			GenerationID: uuidToString(a.GenerationID),
			Kind:         a.Kind,
			MediaURL:     textToPtr(a.MediaUrl),
			Format:       textToPtr(a.Format),
			CreatedAt:    a.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
		})
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
