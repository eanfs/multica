package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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
