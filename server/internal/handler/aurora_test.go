package handler

import (
	"context"
	"errors"
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

// cleanupAuroraSystemAgents removes the workspace's lazily seeded Aurora system
// agents, skills and managed runtime at teardown. Generation creation seeds
// these on first use and they would otherwise persist into every later handler
// test, whose `agent ... LIMIT 1` agent-runtime heuristics assume the workspace
// holds only user-visible agents (kind='system' is excluded by
// GetAgentInWorkspace but not by an unfiltered LIMIT 1). Agents go first so
// their agent_skill rows cascade; skills and the runtime follow once the agents
// no longer reference them.
func cleanupAuroraSystemAgents(t *testing.T) {
	t.Helper()
	names := make([]string, 0, len(aurora.Catalog()))
	for _, e := range aurora.Catalog() {
		names = append(names, e.Name)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := testPool.Exec(ctx,
			`DELETE FROM agent WHERE workspace_id = $1 AND system_key LIKE 'aurora:%'`, testWorkspaceID); err != nil {
			t.Errorf("cleanup aurora agents: %v", err)
		}
		if _, err := testPool.Exec(ctx,
			`DELETE FROM skill WHERE workspace_id = $1 AND name = ANY($2::text[])`, testWorkspaceID, names); err != nil {
			t.Errorf("cleanup aurora skills: %v", err)
		}
		if _, err := testPool.Exec(ctx,
			`DELETE FROM agent_runtime WHERE workspace_id = $1 AND daemon_id IS NULL AND provider = 'aurora_managed'`, testWorkspaceID); err != nil {
			t.Errorf("cleanup aurora runtime: %v", err)
		}
	})
}

func TestCreateAuroraGenerationReservesAndEnqueues(t *testing.T) {
	creditTestReset(t)
	cleanupAuroraSystemAgents(t)
	ctx := context.Background()
	user := parseUUID(testUserID)
	ws := parseUUID(testWorkspaceID)

	// Fund the caller so the reservation can succeed. The grant needs no
	// particular size — 1000 credits covers xhs-image's 620 with room to spare
	// and leaves the post-reservation balance easy to assert.
	if err := testHandler.Credit.Grant(ctx, user, ws, 1_000_000_000, aurora.LedgerKindAdjustment, creditRef("seed")); err != nil {
		t.Fatalf("Grant: %v", err)
	}

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
	const wantReserved = 620_000_000 // xhs-image: 620 credits × 1e6 micro
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
	if gen.CreditsReserved != wantReserved {
		t.Fatalf("expected creditsReserved %d, got %d", wantReserved, gen.CreditsReserved)
	}

	// The handler must have written the task's id and the reserved micro-credits
	// back onto the row, and the wallet must reflect the reservation.
	var taskID pgtype.UUID
	var reserved int64
	dbfx.QueryRow(t, `SELECT task_id, credits_reserved FROM aurora_generation WHERE id = $1`, gen.ID).Scan(&taskID, &reserved)
	if !taskID.Valid {
		t.Fatal("expected a non-null task_id on the generation row")
	}
	if reserved != wantReserved {
		t.Fatalf("row credits_reserved = %d, want %d", reserved, wantReserved)
	}
	t.Cleanup(func() {
		if taskID.Valid {
			testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
		}
		testPool.Exec(context.Background(), `DELETE FROM aurora_generation WHERE id = $1`, gen.ID)
	})

	bal, err := testHandler.Credit.Balance(ctx, user)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := int64(1_000_000_000 - wantReserved); bal != want {
		t.Fatalf("balance after reserve = %d, want %d", bal, want)
	}

	// The row must still be scoped to the workspace and user resolved from the
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
}

func TestCreateAuroraGenerationRejectsInsufficientCredits(t *testing.T) {
	creditTestReset(t)
	cleanupAuroraSystemAgents(t)

	req := newRequest(http.MethodPost, "/api/aurora/generations", map[string]string{
		"skillId": "xhs-image",
		"prompt":  "余额不足的生成请求",
	})
	testutil.Call(t, testHandler.CreateAuroraGeneration, req).Want(http.StatusPaymentRequired)

	// The aborted generation must be terminal with the documented reason...
	var status string
	var genErr pgtype.Text
	dbfx.QueryRow(t, `
		SELECT status, error FROM aurora_generation
		WHERE workspace_id = $1 AND skill_id = 'xhs-image'
		ORDER BY created_at DESC LIMIT 1`,
		testWorkspaceID).Scan(&status, &genErr)
	if status != "failed" {
		t.Fatalf("generation status = %q, want failed", status)
	}
	if !genErr.Valid || genErr.String != "insufficient credits" {
		t.Fatalf("generation error = %q, want 'insufficient credits'", genErr.String)
	}

	// ...and nothing may have been enqueued onto any Aurora system agent.
	taskCount := dbfx.Count(t, `
		SELECT count(*) FROM agent_task_queue atq
		JOIN agent a ON a.id = atq.agent_id
		WHERE a.workspace_id = $1 AND a.system_key LIKE 'aurora:%'`,
		testWorkspaceID)
	if taskCount != 0 {
		t.Fatalf("enqueued %d tasks onto Aurora system agents, want 0", taskCount)
	}
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
//
// It clears by user_id, which is why every test's references carry testUserID:
// the idempotency key is globally unique and the fast path looks it up by key
// alone, so a row orphaned by an abnormally killed run (whose fixture user no
// longer exists, and whose id no later run reuses) is invisible to this reset
// and would silently satisfy a later run's fast path. Run-unique references
// make these tests independent of cleanup having happened at all.
func creditTestReset(t *testing.T) {
	t.Helper()
	creditTestResetUser(t, testUserID)
}

// creditTestResetUser is creditTestReset for a wallet other than the suite
// fixture's — the billing endpoints are account-scoped, so a test that proves
// they read the caller's wallet needs a second one.
func creditTestResetUser(t *testing.T, userID string) {
	t.Helper()
	reset := func() {
		ctx := context.Background()
		if _, err := testPool.Exec(ctx, `DELETE FROM credit_ledger WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("reset credit_ledger: %v", err)
		}
		if _, err := testPool.Exec(ctx, `DELETE FROM credit_balance WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("reset credit_balance: %v", err)
		}
	}
	reset()
	t.Cleanup(reset)
}

// creditRef namespaces a test reference with the fixture user's id, so the
// idempotency key derived from it is unique to this run. The key is globally
// unique and adjust's fast path looks it up by key alone, without a user
// filter, so a row left by an abnormally killed run — whose fixture user no
// longer exists and whose id no later run reuses — would otherwise satisfy a
// later run's fast path and turn an operation into a silent no-op. Tying
// references to testUserID makes the tests correct regardless of what a
// previous run left behind.
func creditRef(base string) string {
	return base + "-" + testUserID
}

// creditRefFor is creditRef for a wallet other than the fixture user's. The
// namespace has to be the *owning* user's id, not the caller's: a decoy wallet
// funded with creditRef(base) would carry the same idempotency key as the
// caller's own row with that base, so whichever test ran first would satisfy
// the second one's fast path and the second wallet would silently never be
// funded — the exact staleness creditRef exists to prevent.
func creditRefFor(userID, base string) string {
	return base + "-" + userID
}

func TestAuroraCreditReserveAndRefundAreIdempotent(t *testing.T) {
	creditTestReset(t)
	ctx := context.Background()
	user := parseUUID(testUserID)
	ws := parseUUID(testWorkspaceID)
	gen := creditRef("gen-1")

	if err := testHandler.Credit.Grant(ctx, user, ws, 1000, aurora.LedgerKindAdjustment, creditRef("seed")); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := testHandler.Credit.Reserve(ctx, user, ws, 300, gen); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// Idempotency: re-reserving the same reference must not deduct again.
	if err := testHandler.Credit.Reserve(ctx, user, ws, 300, gen); err != nil {
		t.Fatalf("Reserve retry: %v", err)
	}
	bal, err := testHandler.Credit.Balance(ctx, user)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 700 {
		t.Fatalf("balance after idempotent reserve = %d, want 700", bal)
	}

	if err := testHandler.Credit.Refund(ctx, user, ws, 300, gen); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	// Idempotency: re-refunding the same reference must not credit again.
	if err := testHandler.Credit.Refund(ctx, user, ws, 300, gen); err != nil {
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

	if err := testHandler.Credit.Reserve(ctx, user, ws, 100, creditRef("gen-x")); !errors.Is(err, aurora.ErrInsufficientCredits) {
		t.Fatalf("Reserve with empty balance: err = %v, want ErrInsufficientCredits", err)
	}
}

func TestAuroraCreditGrantRejectsInvalidKind(t *testing.T) {
	creditTestReset(t)
	ctx := context.Background()
	user := parseUUID(testUserID)
	ws := parseUUID(testWorkspaceID)

	if err := testHandler.Credit.Grant(ctx, user, ws, 100, "bogus", creditRef("seed")); err == nil {
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
		"Reserve": func() error { return testHandler.Credit.Reserve(ctx, user, ws, -100, creditRef("gen-neg")) },
		"Refund":  func() error { return testHandler.Credit.Refund(ctx, user, ws, -100, creditRef("gen-neg")) },
		"Grant": func() error {
			return testHandler.Credit.Grant(ctx, user, ws, -100, aurora.LedgerKindTopup, creditRef("gen-neg"))
		},
		"Reserve/zero": func() error { return testHandler.Credit.Reserve(ctx, user, ws, 0, creditRef("gen-zero")) },
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
			// The winner's raw insert and the loser's Reserve must agree on the
			// idempotency key, so both derive it from this one reference. The
			// service computes "reserve:"+reference; the raw statement below
			// spells out the same key.
			raceRef := creditRef("gen-race")
			raceKey := "reserve:" + raceRef

			if err := testHandler.Credit.Grant(ctx, user, ws, tc.seedMicro, aurora.LedgerKindAdjustment, creditRef("seed")); err != nil {
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
				VALUES ($1, $2, 'deduction', -300, $3, $4, $5)`,
				user, ws, winnerBalance, raceRef, raceKey); err != nil {
				t.Fatalf("winner ledger insert: %v", err)
			}

			dupErr := make(chan error, 1)
			go func() {
				dupErr <- testHandler.Credit.Reserve(ctx, user, ws, 300, raceRef)
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
				`SELECT count(*) FROM credit_ledger WHERE idempotency_key = $1`, raceKey).Scan(&ledgerRows); err != nil {
				t.Fatalf("count ledger rows: %v", err)
			}
			if ledgerRows != 1 {
				t.Fatalf("ledger rows for %s = %d, want 1", raceKey, ledgerRows)
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

// auroraBillingResponse is the wire shape the two read-only billing endpoints
// share. Both fields are pointers because the whole point of these tests is to
// tell "the key is absent" from "the key is present and zero" — a value field
// cannot.
type auroraBillingResponse struct {
	AvailableMicro *int64 `json:"availableMicro"`
	Transactions   []struct {
		ID                string `json:"id"`
		Kind              string `json:"kind"`
		AmountMicro       int64  `json:"amountMicro"`
		BalanceAfterMicro int64  `json:"balanceAfterMicro"`
		Reference         string `json:"reference"`
		CreatedAt         string `json:"createdAt"`
	} `json:"transactions"`
}

// TestAuroraBillingBalanceReportsTheCallersWallet pins three things a naive
// implementation gets wrong: the field is present (a user with no wallet row
// still gets a number, not a missing key), it is the *caller's* wallet (the
// endpoints are account-scoped, so a shared or hardcoded balance would pass a
// single-user test), and it follows the ledger rather than drifting from it.
func TestAuroraBillingBalanceReportsTheCallersWallet(t *testing.T) {
	creditTestReset(t)
	ctx := context.Background()
	ws := parseUUID(testWorkspaceID)

	// A second wallet, deliberately funded differently, so a handler that read
	// the wrong account — or any account but the caller's — is visible.
	otherID := dbfx.User(t, "Aurora Billing Other", "aurora-billing-other@multica.test")
	creditTestResetUser(t, otherID)
	if err := testHandler.Credit.Grant(ctx, parseUUID(otherID), ws, 999, aurora.LedgerKindAdjustment, creditRefFor(otherID, "other-seed")); err != nil {
		t.Fatalf("Grant for other user: %v", err)
	}

	// No wallet row at all: 0, present.
	req := newRequest(http.MethodGet, "/api/aurora/billing/balance", nil)
	out := testutil.Decode[auroraBillingResponse](t, testHandler.GetAuroraBillingBalance, req, http.StatusOK)
	if out.AvailableMicro == nil {
		t.Fatal("availableMicro is absent, want 0 for a user with no wallet row")
	}
	if *out.AvailableMicro != 0 {
		t.Fatalf("availableMicro = %d for a fresh wallet, want 0", *out.AvailableMicro)
	}

	// Funded: the number tracks the ledger, and the other user's balance is
	// not what comes back.
	if err := testHandler.Credit.Grant(ctx, parseUUID(testUserID), ws, 2500, aurora.LedgerKindAdjustment, creditRef("seed")); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	out = testutil.Decode[auroraBillingResponse](t, testHandler.GetAuroraBillingBalance, req, http.StatusOK)
	if out.AvailableMicro == nil || *out.AvailableMicro != 2500 {
		t.Fatalf("availableMicro = %v after a 2500 grant, want 2500 (the caller's wallet, not another user's)", out.AvailableMicro)
	}
}

// TestAuroraBillingBalanceRequiresAuthentication covers the failure side of
// requireUserID: without an authenticated user there is no wallet to read, and
// defaulting to some other account's balance would leak it.
func TestAuroraBillingBalanceRequiresAuthentication(t *testing.T) {
	req := newRequest(http.MethodGet, "/api/aurora/billing/balance", nil)
	req.Header.Del("X-User-ID")
	testutil.Call(t, testHandler.GetAuroraBillingBalance, req).Want(http.StatusUnauthorized)
}

// TestAuroraBillingTransactionsListsTheCallersLedger checks the row shape the
// frontend schema consumes (packages/core/aurora/schema.ts), that the newest
// row comes first, and that the list is the caller's ledger only. The empty
// case is asserted too: `transactions` must be `[]`, not `null`, or the zod
// `.default([])` never fires and the UI renders against a null array.
func TestAuroraBillingTransactionsListsTheCallersLedger(t *testing.T) {
	creditTestReset(t)
	ctx := context.Background()
	user := parseUUID(testUserID)
	ws := parseUUID(testWorkspaceID)

	empty := testutil.Decode[auroraBillingResponse](t,
		testHandler.ListAuroraBillingTransactions,
		newRequest(http.MethodGet, "/api/aurora/billing/transactions?limit=50", nil),
		http.StatusOK)
	if empty.Transactions == nil {
		t.Fatal("transactions is null for an empty ledger, want an empty array")
	}
	if len(empty.Transactions) != 0 {
		t.Fatalf("transactions for an empty ledger = %d rows, want 0", len(empty.Transactions))
	}

	// Another user's row, which must not appear in the caller's list.
	otherID := dbfx.User(t, "Aurora Ledger Other", "aurora-ledger-other@multica.test")
	creditTestResetUser(t, otherID)
	if err := testHandler.Credit.Grant(ctx, parseUUID(otherID), ws, 999, aurora.LedgerKindAdjustment, creditRefFor(otherID, "other-seed")); err != nil {
		t.Fatalf("Grant for other user: %v", err)
	}

	gen := creditRef("gen-ledger")
	if err := testHandler.Credit.Grant(ctx, user, ws, 1000, aurora.LedgerKindAdjustment, creditRef("seed")); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := testHandler.Credit.Reserve(ctx, user, ws, 300, gen); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := testHandler.Credit.Refund(ctx, user, ws, 300, gen); err != nil {
		t.Fatalf("Refund: %v", err)
	}

	out := testutil.Decode[auroraBillingResponse](t,
		testHandler.ListAuroraBillingTransactions,
		newRequest(http.MethodGet, "/api/aurora/billing/transactions?limit=50", nil),
		http.StatusOK)
	if len(out.Transactions) != 3 {
		t.Fatalf("transactions = %d rows, want 3 (grant, deduction, refund; not the other user's)", len(out.Transactions))
	}
	// Newest first: the refund was written last.
	wantOrder := []struct {
		kind      string
		amount    int64
		balance   int64
		reference string
	}{
		{aurora.LedgerKindRefund, 300, 1000, gen},
		{aurora.LedgerKindDeduction, -300, 700, gen},
		{aurora.LedgerKindAdjustment, 1000, 1000, creditRef("seed")},
	}
	for i, want := range wantOrder {
		got := out.Transactions[i]
		if got.ID == "" {
			t.Errorf("row %d: id is empty", i)
		}
		if got.Kind != want.kind || got.AmountMicro != want.amount || got.BalanceAfterMicro != want.balance || got.Reference != want.reference {
			t.Errorf("row %d = %+v, want kind=%s amount=%d balanceAfter=%d reference=%s",
				i, got, want.kind, want.amount, want.balance, want.reference)
		}
		if _, err := time.Parse(time.RFC3339Nano, got.CreatedAt); err != nil {
			t.Errorf("row %d: createdAt = %q, want RFC3339: %v", i, got.CreatedAt, err)
		}
	}
}

// TestAuroraBillingTransactionsLimit covers the ?limit contract. The ledger is
// seeded past the endpoint's default and maximum so each case is told apart by
// its row count — a smaller ledger would make "honoured", "defaulted" and
// "rejected" all return the same number and prove nothing.
func TestAuroraBillingTransactionsLimit(t *testing.T) {
	creditTestReset(t)

	// Seeded with raw SQL rather than the service: 60 wallet operations would
	// each open a transaction to prove something only the read path decides.
	dbfx.Exec(t, `
		INSERT INTO credit_ledger (user_id, workspace_id, kind, amount_micro, balance_after_micro, reference, idempotency_key)
		SELECT $1, $2, 'adjustment', 10, 10, 'seed-' || i, $3 || '-bulk-' || i
		FROM generate_series(1, 60) AS i`, testUserID, testWorkspaceID, testUserID)

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"honours a limit inside the range", "?limit=2", 2},
		{"honours the maximum", "?limit=200", 60},
		{"defaults when absent", "", 50},
		{"defaults on junk", "?limit=abc", 50},
		{"defaults on zero", "?limit=0", 50},
		{"defaults on a negative limit", "?limit=-5", 50},
		{"defaults above the maximum", "?limit=201", 50},
		{"defaults on an absurd limit", "?limit=100000", 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := testutil.Decode[auroraBillingResponse](t,
				testHandler.ListAuroraBillingTransactions,
				newRequest(http.MethodGet, "/api/aurora/billing/transactions"+tc.query, nil),
				http.StatusOK)
			if len(out.Transactions) != tc.want {
				t.Fatalf("transactions with %q = %d rows, want %d", tc.query, len(out.Transactions), tc.want)
			}
		})
	}
}

// TestAuroraBillingTransactionsRequiresAuthentication mirrors the balance
// endpoint's failure side.
func TestAuroraBillingTransactionsRequiresAuthentication(t *testing.T) {
	req := newRequest(http.MethodGet, "/api/aurora/billing/transactions", nil)
	req.Header.Del("X-User-ID")
	testutil.Call(t, testHandler.ListAuroraBillingTransactions, req).Want(http.StatusUnauthorized)
}

// TestAuroraBillingRejectsMalformedUserID covers the second half of resolving
// the caller. The auth middleware stamps X-User-ID from a validated token in
// production, but the handlers are also reachable from tests and from any
// future caller that sets the header itself, and the UUID rules in AGENTS.md
// require a request-boundary string to go through parseUUIDOrBadRequest. The
// trusted-input variant, parseUUID, panics on a malformed value — a 400 here
// is the difference between a rejected request and a crashed process.
func TestAuroraBillingRejectsMalformedUserID(t *testing.T) {
	handlers := map[string]http.HandlerFunc{
		"balance":      testHandler.GetAuroraBillingBalance,
		"transactions": testHandler.ListAuroraBillingTransactions,
	}
	for name, handler := range handlers {
		t.Run(name, func(t *testing.T) {
			req := newRequest(http.MethodGet, "/api/aurora/billing/"+name, nil)
			req.Header.Set("X-User-ID", "not-a-uuid")
			testutil.Call(t, handler, req).Want(http.StatusBadRequest)
		})
	}
}
