package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

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
	// Idempotency: re-reserving the same reference must not deduct again.
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
	// Idempotency: re-refunding the same reference must not credit again.
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

// TestAuroraCreditRejectsNonPositiveAmounts guards the direction of every
// wallet movement. Reserve negates its argument, so a negative amount would
// credit the wallet while recording a "deduction" row; Grant passes its
// argument straight through, so a negative amount would debit while recording
// a "topup"/"adjustment" row. Both are silent and direction-inverting, and
// neither would be caught by the balance assertions above.
func TestAuroraCreditRejectsNonPositiveAmounts(t *testing.T) {
	creditTestReset(t)
	ctx := context.Background()
	user := parseUUID(testUserID)
	ws := parseUUID(testWorkspaceID)

	calls := map[string]func() error{
		"Reserve": func() error { return testHandler.Credit.Reserve(ctx, user, ws, -100, "gen-neg") },
		"Refund":  func() error { return testHandler.Credit.Refund(ctx, user, ws, -100, "gen-neg") },
		"Grant": func() error {
			return testHandler.Credit.Grant(ctx, user, ws, -100, aurora.LedgerKindTopup, "gen-neg")
		},
		"Reserve/zero": func() error { return testHandler.Credit.Reserve(ctx, user, ws, 0, "gen-zero") },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s with a non-positive amount should fail", name)
		}
	}

	// Nothing may have been written: no wallet row, no ledger row.
	bal, err := testHandler.Credit.Balance(ctx, user)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance after rejected calls = %d, want 0", bal)
	}
	var ledgerRows int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM credit_ledger WHERE user_id = $1`, testUserID).Scan(&ledgerRows); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if ledgerRows != 0 {
		t.Fatalf("ledger rows after rejected calls = %d, want 0", ledgerRows)
	}
}

// TestAuroraCreditConcurrentDuplicateReserveIsIdempotent pins the two branches
// of adjust that only a real race reaches. Both are the difference between
// charging once and charging twice, and neither is reachable from the
// single-goroutine tests above:
//
//   - enough balance for both: the duplicate's deduct succeeds, so it reaches
//     the ledger insert and loses on the unique index — it must roll back the
//     redundant balance change and report success.
//   - enough balance for one only: the winner's deduction is what leaves too
//     little for the duplicate, so the duplicate's conditional deduct matches
//     no row — it must not report that as insufficient credits.
//
// In both cases the duplicate carries the same reference, hence the same
// idempotency key. A distinct key would prove nothing here.
//
// The interleaving is sequenced, not timing-dependent: a transaction holds the
// credit_balance row lock and the duplicate is only released once Postgres
// reports it waiting, so the race is guaranteed rather than hoped for. The
// winner is driven with raw SQL because its transaction has to stay open across
// the duplicate's arrival, which the service API cannot express; those
// statements mirror what adjust's winning transaction does. The behaviour under
// test is the duplicate's.
func TestAuroraCreditConcurrentDuplicateReserveIsIdempotent(t *testing.T) {
	cases := []struct {
		name      string
		seedMicro int64
	}{
		{"duplicate reaches the ledger insert", 1000},
		{"duplicate finds the balance already spent", 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creditTestReset(t)
			ctx := context.Background()
			user := parseUUID(testUserID)
			ws := parseUUID(testWorkspaceID)

			if err := testHandler.Credit.Grant(ctx, user, ws, tc.seedMicro, aurora.LedgerKindAdjustment, "seed"); err != nil {
				t.Fatalf("Grant: %v", err)
			}

			winner, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin winner tx: %v", err)
			}
			defer winner.Rollback(ctx)
			var winnerBalance int64
			if err := winner.QueryRow(ctx, `
				UPDATE credit_balance SET available_micro = available_micro - 300, updated_at = now()
				WHERE user_id = $1 AND available_micro >= 300
				RETURNING available_micro`, user).Scan(&winnerBalance); err != nil {
				t.Fatalf("winner deduct: %v", err)
			}
			if _, err := winner.Exec(ctx, `
				INSERT INTO credit_ledger
					(user_id, workspace_id, kind, amount_micro, balance_after_micro, reference, idempotency_key)
				VALUES ($1, $2, 'deduction', -300, $3, 'gen-race', 'reserve:gen-race')`,
				user, ws, winnerBalance); err != nil {
				t.Fatalf("winner ledger insert: %v", err)
			}

			dupErr := make(chan error, 1)
			go func() {
				dupErr <- testHandler.Credit.Reserve(ctx, user, ws, 300, "gen-race")
			}()

			// The winner is still uncommitted, so the duplicate's pre-check
			// missed and it is parked inside its own transaction. Releasing the
			// winner only after observing that wait is what makes the ordering
			// deterministic.
			waitForCreditBalanceLock(t, ctx)

			if err := winner.Commit(ctx); err != nil {
				t.Fatalf("commit winner: %v", err)
			}

			if err := <-dupErr; err != nil {
				t.Fatalf("duplicate Reserve = %v, want nil (the operation already succeeded)", err)
			}

			bal, err := testHandler.Credit.Balance(ctx, user)
			if err != nil {
				t.Fatalf("Balance: %v", err)
			}
			wantBal := tc.seedMicro - 300
			if bal != wantBal {
				t.Fatalf("balance after concurrent duplicate = %d, want %d (deducted exactly once)", bal, wantBal)
			}
			// Exactly one ledger row for the contested key: the winner's. The
			// seed grant has its own key and is not part of this claim.
			var ledgerRows int
			if err := testPool.QueryRow(ctx,
				`SELECT count(*) FROM credit_ledger WHERE idempotency_key = 'reserve:gen-race'`).Scan(&ledgerRows); err != nil {
				t.Fatalf("count ledger rows: %v", err)
			}
			if ledgerRows != 1 {
				t.Fatalf("ledger rows for reserve:gen-race = %d, want 1", ledgerRows)
			}
		})
	}
}

// waitForCreditBalanceLock blocks until a backend other than this one is
// waiting on a lock while touching credit_balance, or fails the test after a
// bounded wait so a regression surfaces as a failure rather than a hang.
func waitForCreditBalanceLock(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked int
		if err := testPool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%credit_balance%'`).Scan(&blocked); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if blocked > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("duplicate Reserve never blocked on the credit_balance row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
