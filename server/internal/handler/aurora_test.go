package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestListAuroraSkills(t *testing.T) {
	req := newRequest(http.MethodGet, "/api/aurora/skills", nil)
	var out struct {
		Skills []struct {
			ID        string   `json:"id"`
			Name      string   `json:"name"`
			NameEn    string   `json:"name_en"`
			Category  string   `json:"category"`
			Credits   int      `json:"credits"`
			Input     []string `json:"input"`
			Output    []string `json:"output"`
			Featured  *bool    `json:"featured"`
			Available *bool    `json:"available"`
		} `json:"skills"`
	}
	response := testutil.Call(t, testHandler.ListAuroraSkills, req).Want(http.StatusOK)
	response.JSON(&out)
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if len(out.Skills) != 16 {
		t.Fatalf("expected 16 skills, got %d", len(out.Skills))
	}
	available := 0
	for _, s := range out.Skills {
		if s.ID == "" || s.Name == "" || s.NameEn == "" || s.Category == "" || s.Credits <= 0 || len(s.Input) == 0 || len(s.Output) == 0 {
			t.Errorf("skill response missing required metadata: %#v", s)
		}
		// False values must be present, not omitted or null, for API consumers.
		if s.Featured == nil || s.Available == nil {
			t.Errorf("skill %q missing featured or available boolean", s.ID)
		}
		if s.Available != nil && *s.Available {
			available++
		}
	}
	if available != 13 {
		t.Errorf("expected 13 available skills in response, got %d", available)
	}
	poster := out.Skills[0]
	if poster.ID != "poster" || poster.Name != "海报制作" || poster.NameEn != "Poster" || poster.Category != "image" || poster.Credits != 760 ||
		!reflect.DeepEqual(poster.Input, []string{"text", "image"}) || !reflect.DeepEqual(poster.Output, []string{"image"}) ||
		poster.Featured == nil || !*poster.Featured || poster.Available == nil || !*poster.Available {
		t.Errorf("unexpected poster response: %#v", poster)
	}
}

func TestCreateAuroraGeneration(t *testing.T) {
	prompt := "生成一张新加坡亲子游封面"
	req := newRequest(http.MethodPost, "/api/aurora/generations", map[string]string{
		"skillId": "xhs-image",
		"prompt":  prompt,
	})
	out := testutil.Decode[struct {
		Generation struct {
			ID              string `json:"id"`
			SkillID         string `json:"skillId"`
			Prompt          string `json:"prompt"`
			Status          string `json:"status"`
			CreditsReserved int64  `json:"creditsReserved"`
		} `json:"generation"`
	}](t, testHandler.CreateAuroraGeneration, req, http.StatusCreated)

	gen := out.Generation
	if gen.ID == "" {
		t.Fatalf("expected non-empty id, got %q", gen.ID)
	}
	if gen.Status != "queued" {
		t.Fatalf("expected queued, got %q", gen.Status)
	}
	if gen.SkillID != "xhs-image" {
		t.Fatalf("expected skillId xhs-image, got %q", gen.SkillID)
	}
	if gen.Prompt != prompt {
		t.Fatalf("expected prompt %q, got %q", prompt, gen.Prompt)
	}
	if gen.CreditsReserved != 0 {
		t.Fatalf("expected creditsReserved 0, got %d", gen.CreditsReserved)
	}

	// The row must be scoped to the workspace and user resolved from the
	// request headers, not zero UUIDs (the historical #1661 bug class).
	var wsID, userID pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`SELECT workspace_id, user_id FROM aurora_generation WHERE id = $1`, gen.ID,
	).Scan(&wsID, &userID); err != nil {
		t.Fatalf("load generation row: %v", err)
	}
	if wsID.String() != testWorkspaceID || userID.String() != testUserID {
		t.Fatalf("row scoped to workspace/user %s/%s, want %s/%s", wsID.String(), userID.String(), testWorkspaceID, testUserID)
	}

	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM aurora_generation WHERE id = $1`, gen.ID); err != nil {
			t.Errorf("cleanup generation row: %v", err)
		}
	})
}

func TestCreateAuroraGenerationRejectsUnknownSkill(t *testing.T) {
	req := newRequest(http.MethodPost, "/api/aurora/generations", map[string]string{
		"skillId": "nope",
		"prompt":  "x",
	})
	testutil.Call(t, testHandler.CreateAuroraGeneration, req).Want(http.StatusBadRequest)
}

func TestCreateAuroraGenerationRejectsUnavailableSkill(t *testing.T) {
	// avatar-video is phase-2 (spec §9.2): listed in the catalog but not
	// submittable until its execution path exists.
	req := newRequest(http.MethodPost, "/api/aurora/generations", map[string]string{
		"skillId": "avatar-video",
		"prompt":  "x",
	})
	testutil.Call(t, testHandler.CreateAuroraGeneration, req).Want(http.StatusBadRequest)
}

func TestCreateAuroraGenerationRejectsMissingPrompt(t *testing.T) {
	req := newRequest(http.MethodPost, "/api/aurora/generations", map[string]string{
		"skillId": "xhs-image",
	})
	testutil.Call(t, testHandler.CreateAuroraGeneration, req).Want(http.StatusBadRequest)
}

func TestCreateAuroraGenerationRejectsMissingSkillID(t *testing.T) {
	req := newRequest(http.MethodPost, "/api/aurora/generations", map[string]string{
		"prompt": "x",
	})
	testutil.Call(t, testHandler.CreateAuroraGeneration, req).Want(http.StatusBadRequest)
}

func TestCreateAuroraGenerationRejectsMalformedBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/aurora/generations", strings.NewReader(`{"skillId":`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	testutil.Call(t, testHandler.CreateAuroraGeneration, req).Want(http.StatusBadRequest)
}

// creditTestReset empties the fixture user's credit wallet so each test starts
// from a known-empty state, and empties it again on cleanup. credit_balance and
// credit_ledger carry no foreign key to user, so the row fixture's own cleanup
// would otherwise leave these rows behind.
func creditTestReset(t *testing.T) {
	t.Helper()
	reset := func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM credit_ledger WHERE user_id = $1`, testUserID)
		_, _ = testPool.Exec(ctx, `DELETE FROM credit_balance WHERE user_id = $1`, testUserID)
	}
	reset()
	t.Cleanup(reset)
}

func TestAuroraCreditReserveAndRefundAreIdempotent(t *testing.T) {
	creditTestReset(t)
	ctx := context.Background()
	user := parseUUID(testUserID)
	ws := parseUUID(testWorkspaceID)

	if err := testHandler.Credit.Grant(ctx, user, ws, 1000, aurora.LedgerKindAdjustment, "seed"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := testHandler.Credit.Reserve(ctx, user, ws, 300, "gen-1"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// 幂等：重复 reserve 同 reference 不应再扣。
	if err := testHandler.Credit.Reserve(ctx, user, ws, 300, "gen-1"); err != nil {
		t.Fatalf("Reserve retry: %v", err)
	}
	bal, err := testHandler.Credit.Balance(ctx, user)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 700 {
		t.Fatalf("balance after idempotent reserve = %d, want 700", bal)
	}

	if err := testHandler.Credit.Refund(ctx, user, ws, 300, "gen-1"); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	// 幂等：重复 refund 不应再加。
	if err := testHandler.Credit.Refund(ctx, user, ws, 300, "gen-1"); err != nil {
		t.Fatalf("Refund retry: %v", err)
	}
	bal, err = testHandler.Credit.Balance(ctx, user)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 1000 {
		t.Fatalf("balance after refund = %d, want 1000", bal)
	}
}

func TestAuroraCreditReserveFailsWhenInsufficient(t *testing.T) {
	creditTestReset(t)
	ctx := context.Background()
	user := parseUUID(testUserID)
	ws := parseUUID(testWorkspaceID)

	if err := testHandler.Credit.Reserve(ctx, user, ws, 100, "gen-x"); err != aurora.ErrInsufficientCredits {
		t.Fatalf("Reserve with empty balance: err = %v, want ErrInsufficientCredits", err)
	}
}

func TestAuroraCreditGrantRejectsInvalidKind(t *testing.T) {
	creditTestReset(t)
	ctx := context.Background()
	user := parseUUID(testUserID)
	ws := parseUUID(testWorkspaceID)

	if err := testHandler.Credit.Grant(ctx, user, ws, 100, "bogus", "seed"); err == nil {
		t.Fatal("Grant with invalid kind should fail")
	}
}

func TestAuroraCreditBalanceIsZeroWithoutARow(t *testing.T) {
	creditTestReset(t)
	bal, err := testHandler.Credit.Balance(context.Background(), parseUUID(testUserID))
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance for a user with no wallet row = %d, want 0", bal)
	}
}
