package handler

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/storage"
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

	// The caller has no subscription, so this first generation of the month
	// also granted the free tier's monthly credits before reserving.
	bal, err := testHandler.Credit.Balance(ctx, user)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := int64(1_000_000_000) + testHandler.Tiers.FreeMonthlyMicro() - wantReserved; bal != want {
		t.Fatalf("balance after reserve = %d, want %d (seed + free monthly grant - reserved)", bal, want)
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

// resetAuroraGenerations empties the workspace's generation and asset rows so a
// test starts from a known-empty state. Rows are test data only: the fixture
// deletes what it inserts, but a previous test may have aborted mid-run and
// left rows behind.
func resetAuroraGenerations(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `DELETE FROM aurora_generation WHERE workspace_id = $1`, testWorkspaceID)
	dbfx.Exec(t, `DELETE FROM aurora_asset WHERE workspace_id = $1`, testWorkspaceID)
}

// insertGeneration writes a queued generation row and returns its id.
func insertGeneration(t *testing.T, prompt string, over ...testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{
		"workspace_id": testWorkspaceID,
		"user_id":      testUserID,
		"skill_id":     "xhs-image",
		"prompt":       prompt,
		"status":       "queued",
	}
	for _, o := range over {
		maps.Copy(cols, o)
	}
	return dbfx.Insert(t, "aurora_generation", cols)
}

func TestListAuroraGenerationsOrdersAndPages(t *testing.T) {
	resetAuroraGenerations(t)

	oldest := insertGeneration(t, "oldest", testutil.Cols{"created_at": testutil.Raw("now() - interval '3 hours'")})
	middle := insertGeneration(t, "middle", testutil.Cols{"created_at": testutil.Raw("now() - interval '2 hours'")})
	newest := insertGeneration(t, "newest", testutil.Cols{"created_at": testutil.Raw("now() - interval '1 hour'")})

	list := func(path string) []string {
		out := testutil.Decode[struct {
			Generations []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"generations"`
		}](t, testHandler.ListAuroraGenerations, newRequest(http.MethodGet, path, nil), http.StatusOK)
		ids := make([]string, 0, len(out.Generations))
		for _, g := range out.Generations {
			if g.Status != "queued" {
				t.Fatalf("list status = %q, want queued", g.Status)
			}
			ids = append(ids, g.ID)
		}
		return ids
	}

	// Newest first.
	all := list("/api/aurora/generations?limit=50&offset=0")
	if len(all) != 3 || all[0] != newest || all[1] != middle || all[2] != oldest {
		t.Fatalf("order = %v, want [newest middle oldest]", all)
	}

	// Pagination boundary: limit caps the page, offset skips into it.
	page1 := list("/api/aurora/generations?limit=2&offset=0")
	if len(page1) != 2 || page1[0] != newest || page1[1] != middle {
		t.Fatalf("first page = %v, want [newest middle]", page1)
	}
	page2 := list("/api/aurora/generations?limit=2&offset=2")
	if len(page2) != 1 || page2[0] != oldest {
		t.Fatalf("second page = %v, want [oldest]", page2)
	}

	// Junk and out-of-range limits fall back to the default instead of erroring.
	list("/api/aurora/generations?limit=not-a-number&offset=0")
	list("/api/aurora/generations?limit=0&offset=0")
	list("/api/aurora/generations?limit=9999&offset=-5")
}

func TestGetAuroraGenerationIncludesAssets(t *testing.T) {
	resetAuroraGenerations(t)

	genID := insertGeneration(t, "detail prompt", testutil.Cols{
		"status":           "completed",
		"credits_reserved": 620_000_000,
		"credits_charged":  620_000_000,
	})
	dbfx.Insert(t, "aurora_asset", testutil.Cols{
		"generation_id": genID,
		"workspace_id":  testWorkspaceID,
		"kind":          "image",
		"media_url":     "https://cdn.example/out.png",
		"format":        "png",
	})

	req := withURLParam(newRequest(http.MethodGet, "/api/aurora/generations/{id}", nil), "id", genID)
	out := testutil.Decode[struct {
		Generation struct {
			ID              string  `json:"id"`
			SkillID         string  `json:"skillId"`
			Prompt          string  `json:"prompt"`
			Status          string  `json:"status"`
			CreditsReserved int64   `json:"creditsReserved"`
			CreditsCharged  int64   `json:"creditsCharged"`
			Error           *string `json:"error"`
			CreatedAt       string  `json:"createdAt"`
			Assets          []struct {
				ID           string  `json:"id"`
				GenerationID string  `json:"generationId"`
				Kind         string  `json:"kind"`
				MediaURL     *string `json:"mediaUrl"`
				Format       *string `json:"format"`
				CreatedAt    string  `json:"createdAt"`
			} `json:"assets"`
		} `json:"generation"`
	}](t, testHandler.GetAuroraGeneration, req, http.StatusOK)

	g := out.Generation
	if g.ID != genID || g.SkillID != "xhs-image" || g.Prompt != "detail prompt" {
		t.Fatalf("unexpected generation: %#v", g)
	}
	if g.Status != "completed" {
		t.Fatalf("status = %q, want completed", g.Status)
	}
	if g.CreditsReserved != 620_000_000 || g.CreditsCharged != 620_000_000 {
		t.Fatalf("credits = %d reserved / %d charged, want 620000000", g.CreditsReserved, g.CreditsCharged)
	}
	if g.Error != nil {
		t.Fatalf("error = %q, want null", *g.Error)
	}
	if g.CreatedAt == "" {
		t.Fatal("createdAt empty")
	}
	if len(g.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(g.Assets))
	}
	a := g.Assets[0]
	if a.GenerationID != genID || a.Kind != "image" ||
		a.MediaURL == nil || *a.MediaURL != "https://cdn.example/out.png" ||
		a.Format == nil || *a.Format != "png" || a.CreatedAt == "" {
		t.Fatalf("unexpected asset: %#v", a)
	}
}

func TestGetAuroraGenerationDerivesStatusFromTask(t *testing.T) {
	cases := []struct {
		taskStatus string
		want       string
	}{
		{"queued", "queued"},
		{"dispatched", "running"},
		{"running", "running"},
		{"waiting_local_directory", "running"},
		{"deferred", "running"},
		{"completed", "completed"},
		{"failed", "failed"},
		{"cancelled", "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.taskStatus, func(t *testing.T) {
			agentID := dbfx.Agent(t, "Aurora derive "+tc.taskStatus, testRuntimeID)
			taskID := dbfx.Task(t, agentID, testutil.Cols{"status": tc.taskStatus, "runtime_id": testRuntimeID})
			genID := insertGeneration(t, "derive "+tc.taskStatus, testutil.Cols{"task_id": taskID})

			req := withURLParam(newRequest(http.MethodGet, "/api/aurora/generations/{id}", nil), "id", genID)
			out := testutil.Decode[struct {
				Generation struct {
					Status string `json:"status"`
				} `json:"generation"`
			}](t, testHandler.GetAuroraGeneration, req, http.StatusOK)

			if out.Generation.Status != tc.want {
				t.Fatalf("status for task %q = %q, want %q", tc.taskStatus, out.Generation.Status, tc.want)
			}
		})
	}
}

func TestGetAuroraGenerationNotFound(t *testing.T) {
	// Malformed id → 400, not a crash.
	req := withURLParam(newRequest(http.MethodGet, "/api/aurora/generations/{id}", nil), "id", "not-a-uuid")
	testutil.Call(t, testHandler.GetAuroraGeneration, req).Want(http.StatusBadRequest)

	// Well-formed but absent → 404.
	req = withURLParam(newRequest(http.MethodGet, "/api/aurora/generations/{id}", nil), "id", parseUUID(testUserID).String())
	testutil.Call(t, testHandler.GetAuroraGeneration, req).Want(http.StatusNotFound)

	// A generation in another workspace is invisible: the query is scoped by
	// workspace_id, not just id.
	otherWS := dbfx.Workspace(t, "Aurora other workspace", "aurora-gen-other-ws")
	genID := dbfx.Insert(t, "aurora_generation", testutil.Cols{
		"workspace_id": otherWS,
		"user_id":      testUserID,
		"skill_id":     "xhs-image",
		"prompt":       "other workspace",
		"status":       "queued",
	})
	req = withURLParam(newRequest(http.MethodGet, "/api/aurora/generations/{id}", nil), "id", genID)
	testutil.Call(t, testHandler.GetAuroraGeneration, req).Want(http.StatusNotFound)
}

// ---------------------------------------------------------------------------
// Assets — GET /api/aurora/assets, its download, and DELETE
// ---------------------------------------------------------------------------

// auroraAssetBody is the wire shape of one asset, shared by the library list
// and the generation detail endpoint.
type auroraAssetBody struct {
	ID           string  `json:"id"`
	GenerationID string  `json:"generationId"`
	Kind         string  `json:"kind"`
	MediaURL     *string `json:"mediaUrl"`
	Format       *string `json:"format"`
	CreatedAt    string  `json:"createdAt"`
}

type auroraAssetListBody struct {
	Assets []auroraAssetBody `json:"assets"`
}

// insertAsset writes an aurora_asset row and returns its id.
func insertAsset(t *testing.T, generationID string, over ...testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{
		"generation_id": generationID,
		"workspace_id":  testWorkspaceID,
		"kind":          "image",
		"media_url":     "https://cdn.example.com/aurora/out.png",
		"format":        "png",
	}
	for _, o := range over {
		maps.Copy(cols, o)
	}
	return dbfx.Insert(t, "aurora_asset", cols)
}

// insertAssetInOtherWorkspace writes an asset belonging to a workspace the
// caller is not a member of, with its own generation row. Both are invisible
// to the asset endpoints, which is what the cross-workspace cases assert. The
// slug carries a random suffix because workspace slugs are unique: a run that
// aborts before teardown leaves its workspace behind, and a fixed slug would
// make every later run fail on the insert rather than on its own assertion.
func insertAssetInOtherWorkspace(t *testing.T, slug string) string {
	t.Helper()
	otherWS := dbfx.Workspace(t, "Aurora asset workspace "+slug, slug+"-"+uuid.NewString())
	genID := dbfx.Insert(t, "aurora_generation", testutil.Cols{
		"workspace_id": otherWS,
		"user_id":      testUserID,
		"skill_id":     "xhs-image",
		"prompt":       "other workspace asset",
		"status":       "completed",
	})
	return dbfx.Insert(t, "aurora_asset", testutil.Cols{
		"generation_id": genID,
		"workspace_id":  otherWS,
		"kind":          "image",
		"media_url":     "https://cdn.example.com/aurora/secret.png",
		"format":        "png",
	})
}

// withAuroraAssetStorage points the shared handler at an in-memory storage
// backend and restores the previous wiring at cleanup. The shared test handler
// is built without storage, and only the asset download and delete paths reach
// for h.Storage.
func withAuroraAssetStorage(t *testing.T, store storage.Storage) {
	t.Helper()
	origStorage := testHandler.Storage
	origCfg := testHandler.cfg
	origSigner := testHandler.CFSigner
	testHandler.Storage = store
	testHandler.cfg.AttachmentDownloadMode = ""
	testHandler.CFSigner = nil
	t.Cleanup(func() {
		testHandler.Storage = origStorage
		testHandler.cfg = origCfg
		testHandler.CFSigner = origSigner
	})
}

// deleteFailingStorage refuses every object delete, standing in for a storage
// backend that is unreachable at the moment an asset row would be removed.
type deleteFailingStorage struct{ mockStorage }

func (s *deleteFailingStorage) DeleteObject(context.Context, string) error {
	return errors.New("storage unavailable")
}

func listAuroraAssets(t *testing.T, path string, want int) []auroraAssetBody {
	t.Helper()
	return testutil.Decode[auroraAssetListBody](t, testHandler.ListAuroraAssets, newRequest(http.MethodGet, path, nil), want).Assets
}

func TestAuroraAssetListFiltersByGeneration(t *testing.T) {
	resetAuroraGenerations(t)

	genA := insertGeneration(t, "assets A")
	genB := insertGeneration(t, "assets B")
	older := insertAsset(t, genA, testutil.Cols{
		"created_at": testutil.Raw("now() - interval '2 hours'"),
		"kind":       "image",
		"media_url":  "https://cdn.example.com/aurora/older.png",
		"format":     "png",
	})
	newer := insertAsset(t, genA, testutil.Cols{
		"created_at": testutil.Raw("now() - interval '1 hour'"),
		"kind":       "video",
		"media_url":  "https://cdn.example.com/aurora/newer.mp4",
		"format":     "mp4",
	})
	other := insertAsset(t, genB, testutil.Cols{
		"kind":      "document",
		"media_url": "https://cdn.example.com/aurora/other.pdf",
		"format":    "pdf",
	})

	// The unfiltered list is the whole workspace's library, newest first.
	all := listAuroraAssets(t, "/api/aurora/assets?limit=50&offset=0", http.StatusOK)
	if len(all) != 3 || all[0].ID != other || all[1].ID != newer || all[2].ID != older {
		t.Fatalf("workspace order = %v, want [%s %s %s]", assetIDs(all), other, newer, older)
	}

	// generationId narrows the list to one generation's output.
	genAPage := listAuroraAssets(t, "/api/aurora/assets?generationId="+genA+"&limit=50", http.StatusOK)
	if len(genAPage) != 2 || genAPage[0].ID != newer || genAPage[1].ID != older {
		t.Fatalf("generation %s page = %v, want [%s %s]", genA, assetIDs(genAPage), newer, older)
	}
	a := genAPage[0]
	if a.GenerationID != genA || a.Kind != "video" ||
		a.MediaURL == nil || *a.MediaURL != "https://cdn.example.com/aurora/newer.mp4" ||
		a.Format == nil || *a.Format != "mp4" || a.CreatedAt == "" {
		t.Fatalf("unexpected asset: %#v", a)
	}

	genBPage := listAuroraAssets(t, "/api/aurora/assets?generationId="+genB, http.StatusOK)
	if len(genBPage) != 1 || genBPage[0].ID != other {
		t.Fatalf("generation %s page = %v, want [%s]", genB, assetIDs(genBPage), other)
	}

	// Junk and out-of-range paging falls back to the default page rather than
	// erroring, the same contract the generation list holds.
	if len(listAuroraAssets(t, "/api/aurora/assets?limit=not-a-number&offset=-1", http.StatusOK)) != 3 {
		t.Fatal("junk limit did not fall back to the default page")
	}

	// A malformed generation id is a client bug, not an empty library.
	listAuroraAssets(t, "/api/aurora/assets?generationId=not-a-uuid", http.StatusBadRequest)

	// Another workspace's assets stay invisible both with and without a
	// generation filter: the workspace predicate is bound on every path, so
	// naming a foreign generation narrows the caller's list to nothing rather
	// than widening it to that generation's output.
	foreignAsset := insertAssetInOtherWorkspace(t, "aurora-asset-list-other-ws")
	if got := listAuroraAssets(t, "/api/aurora/assets?generationId="+foreignAsset, http.StatusOK); len(got) != 0 {
		t.Fatalf("cross-workspace generation filter returned %v, want none", assetIDs(got))
	}
	if got := listAuroraAssets(t, "/api/aurora/assets?limit=50", http.StatusOK); len(got) != 3 {
		t.Fatalf("unfiltered list = %v, want only this workspace's three assets", assetIDs(got))
	}
}

func assetIDs(assets []auroraAssetBody) []string {
	ids := make([]string, 0, len(assets))
	for _, a := range assets {
		ids = append(ids, a.ID)
	}
	return ids
}

func TestAuroraAssetDownloadRedirectsToSignedURL(t *testing.T) {
	resetAuroraGenerations(t)
	withAuroraAssetStorage(t, &mockStorage{})

	genID := insertGeneration(t, "download")
	assetID := insertAsset(t, genID, testutil.Cols{"media_url": "https://cdn.example.com/aurora/out.png"})

	req := withURLParam(newRequest(http.MethodGet, "/api/aurora/assets/{id}/download", nil), "id", assetID)
	w := testutil.Call(t, testHandler.DownloadAuroraAsset, req).Want(http.StatusFound)

	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("Location = %q, want a signed storage URL", loc.String())
	}
	// Only the object's key is signed — signing the stored URL would hand a
	// hosted-object URL back to the client verbatim.
	if loc.Path != "/aurora/out.png" {
		t.Fatalf("signed path = %q, want /aurora/out.png", loc.Path)
	}
	// A download saves the file rather than previewing it.
	if disposition := loc.Query().Get("response-content-disposition"); !strings.HasPrefix(disposition, "attachment") || !strings.Contains(disposition, "out.png") {
		t.Fatalf("response-content-disposition = %q, want a forced attachment for out.png", disposition)
	}
}

func TestAuroraAssetDownloadNotFound(t *testing.T) {
	resetAuroraGenerations(t)
	withAuroraAssetStorage(t, &mockStorage{})

	// Malformed id → 400, not a crash.
	req := withURLParam(newRequest(http.MethodGet, "/api/aurora/assets/{id}/download", nil), "id", "not-a-uuid")
	testutil.Call(t, testHandler.DownloadAuroraAsset, req).Want(http.StatusBadRequest)

	// Well-formed but absent → 404.
	req = withURLParam(newRequest(http.MethodGet, "/api/aurora/assets/{id}/download", nil), "id", parseUUID(testUserID).String())
	testutil.Call(t, testHandler.DownloadAuroraAsset, req).Want(http.StatusNotFound)

	// An asset whose row never got a media_url has no object to serve.
	genID := insertGeneration(t, "download without media")
	emptyURL := insertAsset(t, genID, testutil.Cols{"media_url": ""})
	req = withURLParam(newRequest(http.MethodGet, "/api/aurora/assets/{id}/download", nil), "id", emptyURL)
	testutil.Call(t, testHandler.DownloadAuroraAsset, req).Want(http.StatusNotFound)

	// Another workspace's asset is invisible: the lookup is scoped by
	// workspace_id, so its id is not an existence oracle.
	foreignAsset := insertAssetInOtherWorkspace(t, "aurora-asset-download-other-ws")
	req = withURLParam(newRequest(http.MethodGet, "/api/aurora/assets/{id}/download", nil), "id", foreignAsset)
	testutil.Call(t, testHandler.DownloadAuroraAsset, req).Want(http.StatusNotFound)
}

// TestAuroraAssetDownloadStreamsWhenThereIsNoSignedURL covers the deployment
// shape with no signable URL to redirect to (local disk, private object host):
// the object is streamed through the API instead, with the same forced-
// attachment disposition the redirect path ends up with.
func TestAuroraAssetDownloadStreamsWhenThereIsNoSignedURL(t *testing.T) {
	resetAuroraGenerations(t)
	body := []byte("id,total\n1,42\n")
	store := &mockStorage{files: map[string][]byte{"aurora/report.csv": body}}
	withAuroraAssetStorage(t, store)
	testHandler.cfg.AttachmentDownloadMode = "proxy"

	genID := insertGeneration(t, "stream download")
	assetID := insertAsset(t, genID, testutil.Cols{
		"kind":      "document",
		"media_url": "https://cdn.example.com/aurora/report.csv",
		"format":    "csv",
	})

	req := withURLParam(newRequest(http.MethodGet, "/api/aurora/assets/{id}/download", nil), "id", assetID)
	w := testutil.Call(t, testHandler.DownloadAuroraAsset, req).Want(http.StatusOK)
	if !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatalf("body = %q, want the stored object", w.Body.String())
	}
	if disposition := w.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment") {
		t.Fatalf("Content-Disposition = %q, want a forced attachment", disposition)
	}

	// The row points at an object the backend no longer has: 404, not an empty
	// 200 file the user would save as garbage.
	missing := insertAsset(t, genID, testutil.Cols{"media_url": "https://cdn.example.com/aurora/gone.png"})
	req = withURLParam(newRequest(http.MethodGet, "/api/aurora/assets/{id}/download", nil), "id", missing)
	testutil.Call(t, testHandler.DownloadAuroraAsset, req).Want(http.StatusNotFound)
}

func TestAuroraAssetDeleteRemovesRowAndObject(t *testing.T) {
	resetAuroraGenerations(t)
	store := &mockStorage{files: map[string][]byte{"aurora/doomed.png": []byte("bytes")}}
	withAuroraAssetStorage(t, store)

	genID := insertGeneration(t, "delete me")
	assetID := insertAsset(t, genID, testutil.Cols{"media_url": "https://cdn.example.com/aurora/doomed.png"})

	req := withURLParam(newRequest(http.MethodDelete, "/api/aurora/assets/{id}", nil), "id", assetID)
	testutil.Call(t, testHandler.DeleteAuroraAsset, req).Want(http.StatusNoContent)

	// The row is gone from the generation's asset list...
	if got := listAuroraAssets(t, "/api/aurora/assets?generationId="+genID, http.StatusOK); len(got) != 0 {
		t.Fatalf("assets after delete = %v, want none", assetIDs(got))
	}
	// ...and so is the stored object: no cascade exists between them, so
	// leaving the file behind would strand it with nothing pointing at it.
	if _, ok := store.files["aurora/doomed.png"]; ok {
		t.Fatal("stored object survived the asset delete")
	}

	// Deleting again is a 404: the asset is already gone.
	testutil.Call(t, testHandler.DeleteAuroraAsset, req).Want(http.StatusNotFound)

	// Malformed id → 400.
	req = withURLParam(newRequest(http.MethodDelete, "/api/aurora/assets/{id}", nil), "id", "not-a-uuid")
	testutil.Call(t, testHandler.DeleteAuroraAsset, req).Want(http.StatusBadRequest)
}

func TestAuroraAssetDeleteLeavesOtherWorkspacesAlone(t *testing.T) {
	resetAuroraGenerations(t)
	withAuroraAssetStorage(t, &mockStorage{})

	foreignAsset := insertAssetInOtherWorkspace(t, "aurora-asset-delete-other-ws")
	req := withURLParam(newRequest(http.MethodDelete, "/api/aurora/assets/{id}", nil), "id", foreignAsset)
	testutil.Call(t, testHandler.DeleteAuroraAsset, req).Want(http.StatusNotFound)

	if n := dbfx.Count(t, `SELECT count(*) FROM aurora_asset WHERE id = $1`, foreignAsset); n != 1 {
		t.Fatalf("cross-workspace delete removed the row (count = %d, want 1)", n)
	}
}

// TestAuroraAssetDeleteWithoutStorage covers a deployment whose storage is not
// wired up: there is no object to reclaim, but the row still goes, so the
// library is not permanently stuck with an asset nobody can remove.
func TestAuroraAssetDeleteWithoutStorage(t *testing.T) {
	resetAuroraGenerations(t)

	genID := insertGeneration(t, "delete without storage")
	assetID := insertAsset(t, genID)

	req := withURLParam(newRequest(http.MethodDelete, "/api/aurora/assets/{id}", nil), "id", assetID)
	testutil.Call(t, testHandler.DeleteAuroraAsset, req).Want(http.StatusNoContent)

	if n := dbfx.Count(t, `SELECT count(*) FROM aurora_asset WHERE id = $1`, assetID); n != 0 {
		t.Fatalf("row survived a delete with no storage configured (count = %d, want 0)", n)
	}
}

// TestAuroraAssetDeleteKeepsRowWhenObjectDeleteFails pins the failure order:
// the row is the only record that the object exists, so an object delete that
// cannot be completed must fail the request and leave the row for a retry
// rather than deleting the row and stranding the file.
func TestAuroraAssetDeleteKeepsRowWhenObjectDeleteFails(t *testing.T) {
	resetAuroraGenerations(t)
	withAuroraAssetStorage(t, &deleteFailingStorage{})

	genID := insertGeneration(t, "delete failure")
	assetID := insertAsset(t, genID)

	req := withURLParam(newRequest(http.MethodDelete, "/api/aurora/assets/{id}", nil), "id", assetID)
	testutil.Call(t, testHandler.DeleteAuroraAsset, req).Want(http.StatusInternalServerError)

	if n := dbfx.Count(t, `SELECT count(*) FROM aurora_asset WHERE id = $1`, assetID); n != 1 {
		t.Fatalf("row removed despite the storage failure (count = %d, want 1)", n)
	}
}

// ---------------------------------------------------------------------------
// Billing — checkout and the Stripe webhook (Plan 5 Task 3)
// ---------------------------------------------------------------------------

// fakePayments stands in for Stripe. The handler tests never reach the network:
// checkout URLs and period ends are whatever the test set, and webhook events
// come from a queue the test fills, so a test can drive an event sequence
// without a signing secret or a real payload.
type fakePayments struct {
	checkoutURL string
	periodEnd   time.Time
	events      []aurora.Event
	// checkoutErr, when set, is what both checkout methods return instead of a
	// URL — the shape of a Stripe outage.
	checkoutErr error
	// eventErr, when set, is what ConstructEvent returns — an invalid
	// signature, without needing to forge one.
	eventErr error
}

func (f *fakePayments) CreateSubscriptionCheckout(ctx context.Context, priceID, successURL, cancelURL, userID, tier string) (string, error) {
	if f.checkoutErr != nil {
		return "", f.checkoutErr
	}
	return f.checkoutURL, nil
}

func (f *fakePayments) CreateTopupCheckout(ctx context.Context, priceID, successURL, cancelURL, userID, topupID string) (string, error) {
	if f.checkoutErr != nil {
		return "", f.checkoutErr
	}
	return f.checkoutURL, nil
}

func (f *fakePayments) GetSubscriptionPeriodEnd(ctx context.Context, subscriptionID string) (time.Time, error) {
	return f.periodEnd, nil
}

func (f *fakePayments) ConstructEvent(payload []byte, sigHeader string) (aurora.Event, error) {
	if f.eventErr != nil {
		return aurora.Event{}, f.eventErr
	}
	if len(f.events) == 0 {
		return aurora.Event{}, errors.New("fakePayments: no queued event")
	}
	event := f.events[0]
	f.events = f.events[1:]
	return event, nil
}

// installAuroraPayments swaps in a fake provider and a catalog that actually
// has price ids. The suite's shared handler is built without Stripe config, so
// its catalog's price ids are empty and every checkout would fail closed — the
// tests that assert a checkout succeeds must supply priced tiers, and the tests
// that assert fail-closed behavior keep the shared handler as it is.
func installAuroraPayments(t *testing.T, fp aurora.PaymentProvider) {
	t.Helper()
	oldPayments, oldTiers := testHandler.Payments, testHandler.Tiers
	testHandler.Payments = fp
	testHandler.Tiers = aurora.NewTierCatalog(
		"price_creator_monthly", "price_creator_yearly",
		"price_pro_monthly", "price_pro_yearly",
		"price_topup_5", "price_topup_20",
	)
	t.Cleanup(func() {
		testHandler.Payments = oldPayments
		testHandler.Tiers = oldTiers
	})
}

// auroraSubscriptionTestReset empties the fixture user's subscription row so a
// test starts from "never subscribed" and leaves nothing behind for the next
// one. aurora_subscription carries no foreign key to user, so the fixture's own
// cleanup would not remove it.
func auroraSubscriptionTestReset(t *testing.T) {
	t.Helper()
	reset := func() {
		if _, err := testPool.Exec(context.Background(),
			`DELETE FROM aurora_subscription WHERE user_id = $1`, testUserID); err != nil {
			t.Fatalf("reset aurora_subscription: %v", err)
		}
	}
	reset()
	t.Cleanup(reset)
}

// auroraSubscriptionRow reads the fixture user's subscription row, failing the
// test when there is none.
func auroraSubscriptionRow(t *testing.T) (tier, status string, periodEnd *time.Time) {
	t.Helper()
	var end *time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT tier, status, current_period_end FROM aurora_subscription WHERE user_id = $1`,
		testUserID).Scan(&tier, &status, &end); err != nil {
		t.Fatalf("load aurora_subscription row: %v", err)
	}
	return tier, status, end
}

// postStripeWebhook drives the public webhook endpoint. The signature header is
// present but meaningless: the fake provider is what decides whether the event
// verifies, so these tests exercise routing and accounting, not crypto.
func postStripeWebhook(t *testing.T, raw string, want int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/aurora/billing/stripe/webhook", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", "t=1,v1=fake")
	testutil.Call(t, testHandler.StripeWebhook, req).Want(want)
}

// checkoutBody is the request body the billing checkout endpoints accept.
func checkoutBody(extra map[string]string) map[string]string {
	body := map[string]string{
		"successUrl": "https://aurora.example.com/ws-1/billing?checkout=success",
		"cancelUrl":  "https://aurora.example.com/ws-1/billing?checkout=cancel",
	}
	maps.Copy(body, extra)
	return body
}

func TestCreateSubscriptionCheckout(t *testing.T) {
	auroraSubscriptionTestReset(t)
	fp := &fakePayments{checkoutURL: "https://checkout.stripe.com/c/test"}
	installAuroraPayments(t, fp)

	req := newRequest(http.MethodPost, "/api/aurora/billing/checkout", checkoutBody(map[string]string{
		"tier": "creator", "billingCycle": "monthly",
	}))
	out := testutil.Decode[struct {
		CheckoutURL string `json:"checkoutUrl"`
	}](t, testHandler.CreateAuroraCheckout, req, http.StatusOK)
	if out.CheckoutURL != fp.checkoutURL {
		t.Fatalf("checkoutUrl = %q, want %q", out.CheckoutURL, fp.checkoutURL)
	}
}

func TestCreateSubscriptionCheckoutRequiresReturnURLs(t *testing.T) {
	installAuroraPayments(t, &fakePayments{checkoutURL: "https://checkout.stripe.com/c/test"})
	for name, body := range map[string]map[string]string{
		"missing successUrl": {"tier": "creator", "cancelUrl": "https://aurora.example.com/cancel"},
		"missing cancelUrl":  {"tier": "creator", "successUrl": "https://aurora.example.com/success"},
		"relative url":       {"tier": "creator", "successUrl": "/success", "cancelUrl": "/cancel"},
		"javascript url":     {"tier": "creator", "successUrl": "javascript:alert(1)", "cancelUrl": "https://aurora.example.com/cancel"},
	} {
		t.Run(name, func(t *testing.T) {
			req := newRequest(http.MethodPost, "/api/aurora/billing/checkout", body)
			testutil.Call(t, testHandler.CreateAuroraCheckout, req).Want(http.StatusBadRequest)
		})
	}
}

// A deployment with no Stripe keys must not pretend it can take money: the
// endpoint answers 503 rather than a checkout URL nothing will honour.
func TestCreateSubscriptionCheckoutFailsClosedWithoutPayments(t *testing.T) {
	old := testHandler.Payments
	testHandler.Payments = nil
	t.Cleanup(func() { testHandler.Payments = old })

	req := newRequest(http.MethodPost, "/api/aurora/billing/checkout", checkoutBody(map[string]string{
		"tier": "creator", "billingCycle": "monthly",
	}))
	testutil.Call(t, testHandler.CreateAuroraCheckout, req).Want(http.StatusServiceUnavailable)
}

func TestCreateSubscriptionCheckoutRejectsUnknownTier(t *testing.T) {
	installAuroraPayments(t, &fakePayments{checkoutURL: "https://checkout.stripe.com/c/test"})
	for _, tier := range []string{"enterprise", "free", ""} {
		req := newRequest(http.MethodPost, "/api/aurora/billing/checkout", checkoutBody(map[string]string{
			"tier": tier, "billingCycle": "monthly",
		}))
		testutil.Call(t, testHandler.CreateAuroraCheckout, req).Want(http.StatusBadRequest)
	}
}

// A tier whose price id is not configured fails closed rather than charging
// whatever price Stripe's default happens to be.
func TestCreateSubscriptionCheckoutRejectsUnconfiguredPrice(t *testing.T) {
	oldPayments, oldTiers := testHandler.Payments, testHandler.Tiers
	testHandler.Payments = &fakePayments{checkoutURL: "https://checkout.stripe.com/c/test"}
	testHandler.Tiers = aurora.NewTierCatalog("", "", "", "", "", "")
	t.Cleanup(func() {
		testHandler.Payments = oldPayments
		testHandler.Tiers = oldTiers
	})

	req := newRequest(http.MethodPost, "/api/aurora/billing/checkout", checkoutBody(map[string]string{
		"tier": "creator", "billingCycle": "monthly",
	}))
	testutil.Call(t, testHandler.CreateAuroraCheckout, req).Want(http.StatusServiceUnavailable)
}

// MVP semantics: no plan switching while a live subscription exists. The user
// is told they already have one rather than being charged twice.
func TestCreateSubscriptionCheckoutRejectsActiveSubscription(t *testing.T) {
	auroraSubscriptionTestReset(t)
	installAuroraPayments(t, &fakePayments{checkoutURL: "https://checkout.stripe.com/c/test"})
	dbfx.Insert(t, "aurora_subscription", testutil.Cols{
		"user_id": testUserID, "tier": "creator", "status": "active",
	})

	req := newRequest(http.MethodPost, "/api/aurora/billing/checkout", checkoutBody(map[string]string{
		"tier": "pro", "billingCycle": "monthly",
	}))
	testutil.Call(t, testHandler.CreateAuroraCheckout, req).Want(http.StatusConflict)
}

// A canceled subscription is not a live one, so the user may subscribe again —
// the webhook's upsert overwrites the canceled row.
func TestCreateSubscriptionCheckoutAllowsResubscribeAfterCancel(t *testing.T) {
	auroraSubscriptionTestReset(t)
	fp := &fakePayments{checkoutURL: "https://checkout.stripe.com/c/test"}
	installAuroraPayments(t, fp)
	dbfx.Insert(t, "aurora_subscription", testutil.Cols{
		"user_id": testUserID, "tier": "creator", "status": "canceled",
	})

	req := newRequest(http.MethodPost, "/api/aurora/billing/checkout", checkoutBody(map[string]string{
		"tier": "creator", "billingCycle": "monthly",
	}))
	out := testutil.Decode[struct {
		CheckoutURL string `json:"checkoutUrl"`
	}](t, testHandler.CreateAuroraCheckout, req, http.StatusOK)
	if out.CheckoutURL != fp.checkoutURL {
		t.Fatalf("checkoutUrl = %q, want %q", out.CheckoutURL, fp.checkoutURL)
	}
}

func TestCreateTopupCheckout(t *testing.T) {
	fp := &fakePayments{checkoutURL: "https://checkout.stripe.com/c/topup"}
	installAuroraPayments(t, fp)

	req := newRequest(http.MethodPost, "/api/aurora/billing/topup/checkout", checkoutBody(map[string]string{
		"topupId": "t5",
	}))
	out := testutil.Decode[struct {
		CheckoutURL string `json:"checkoutUrl"`
	}](t, testHandler.CreateAuroraTopupCheckout, req, http.StatusOK)
	if out.CheckoutURL != fp.checkoutURL {
		t.Fatalf("checkoutUrl = %q, want %q", out.CheckoutURL, fp.checkoutURL)
	}
}

func TestCreateTopupCheckoutRejectsUnknownTopup(t *testing.T) {
	installAuroraPayments(t, &fakePayments{checkoutURL: "https://checkout.stripe.com/c/topup"})
	req := newRequest(http.MethodPost, "/api/aurora/billing/topup/checkout", checkoutBody(map[string]string{
		"topupId": "t50",
	}))
	testutil.Call(t, testHandler.CreateAuroraTopupCheckout, req).Want(http.StatusBadRequest)
}

func TestCreateTopupCheckoutFailsClosedWithoutPayments(t *testing.T) {
	old := testHandler.Payments
	testHandler.Payments = nil
	t.Cleanup(func() { testHandler.Payments = old })

	req := newRequest(http.MethodPost, "/api/aurora/billing/topup/checkout", checkoutBody(map[string]string{
		"topupId": "t5",
	}))
	testutil.Call(t, testHandler.CreateAuroraTopupCheckout, req).Want(http.StatusServiceUnavailable)
}

// The webhook is public, so an unverified signature must be refused before any
// accounting happens.
func TestStripeWebhookRejectsBadSignature(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	installAuroraPayments(t, &fakePayments{eventErr: errors.New("invalid signature")})

	postStripeWebhook(t, `{"type":"checkout.session.completed"}`, http.StatusBadRequest)

	bal, err := testHandler.Credit.Balance(context.Background(), parseUUID(testUserID))
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance = %d after a rejected webhook, want 0", bal)
	}
}

func TestStripeWebhookFailsClosedWithoutPayments(t *testing.T) {
	old := testHandler.Payments
	testHandler.Payments = nil
	t.Cleanup(func() { testHandler.Payments = old })

	postStripeWebhook(t, `{"type":"checkout.session.completed"}`, http.StatusServiceUnavailable)
}

// A subscription checkout persists the plan and grants the first month
// immediately, so the buyer can generate without waiting for the settlement
// loop.
func TestStripeWebhookSubscriptionCompleted(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	periodEnd := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	installAuroraPayments(t, &fakePayments{
		periodEnd: periodEnd,
		events: []aurora.Event{{
			ID:   "evt_sub_1",
			Type: "checkout.session.completed",
			Raw: []byte(`{"id":"cs_1","mode":"subscription","customer":{"id":"cus_1"},` +
				`"subscription":{"id":"sub_1"},"metadata":{"userId":"` + testUserID + `","tier":"creator"}}`),
		}},
	})

	postStripeWebhook(t, `{"type":"checkout.session.completed"}`, http.StatusOK)

	tier, status, end := auroraSubscriptionRow(t)
	if tier != "creator" || status != "active" {
		t.Fatalf("subscription row = %s/%s, want creator/active", tier, status)
	}
	if end == nil || !end.UTC().Equal(periodEnd) {
		t.Fatalf("current_period_end = %v, want %v", end, periodEnd)
	}
	bal, err := testHandler.Credit.Balance(context.Background(), parseUUID(testUserID))
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := int64(3000) * microCreditsPerCredit; bal != want {
		t.Fatalf("balance = %d, want %d (one month of creator credits)", bal, want)
	}
}

// A topup credits the wallet keyed by the Stripe event id, so a redelivery of
// the same event is a no-op rather than a second purchase.
func TestStripeWebhookTopupCompleted(t *testing.T) {
	creditTestReset(t)
	event := aurora.Event{
		ID:   "evt_topup_1",
		Type: "checkout.session.completed",
		Raw:  []byte(`{"id":"cs_2","mode":"payment","metadata":{"userId":"` + testUserID + `","topupId":"t5"}}`),
	}
	installAuroraPayments(t, &fakePayments{events: []aurora.Event{event, event}})

	postStripeWebhook(t, `{"type":"checkout.session.completed"}`, http.StatusOK)
	bal, err := testHandler.Credit.Balance(context.Background(), parseUUID(testUserID))
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := int64(5000) * microCreditsPerCredit; bal != want {
		t.Fatalf("balance = %d, want %d", bal, want)
	}

	// Replay: Stripe redelivers on any non-2xx, so the same event must not
	// credit twice.
	postStripeWebhook(t, `{"type":"checkout.session.completed"}`, http.StatusOK)
	replayed, err := testHandler.Credit.Balance(context.Background(), parseUUID(testUserID))
	if err != nil {
		t.Fatalf("Balance after replay: %v", err)
	}
	if replayed != bal {
		t.Fatalf("replayed topup changed the balance: before=%d after=%d", bal, replayed)
	}
}

// The userId in an event's metadata arrives from a third party. A malformed one
// is acked (so Stripe stops redelivering a permanently-bad event) and changes
// nothing.
func TestStripeWebhookIgnoresMalformedUserMetadata(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	installAuroraPayments(t, &fakePayments{
		periodEnd: time.Now().Add(30 * 24 * time.Hour),
		events: []aurora.Event{{
			ID:   "evt_bad_user",
			Type: "checkout.session.completed",
			Raw:  []byte(`{"id":"cs_3","mode":"payment","metadata":{"userId":"not-a-uuid","topupId":"t5"}}`),
		}},
	})

	postStripeWebhook(t, `{"type":"checkout.session.completed"}`, http.StatusOK)

	var rows int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM aurora_subscription WHERE user_id = $1`, testUserID).Scan(&rows); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if rows != 0 {
		t.Fatalf("subscription rows = %d after a malformed-metadata event, want 0", rows)
	}
}

// A subscription update moves the stored row in step with Stripe without
// granting anything: grants are the settlement loop's and the checkout
// event's job.
func TestStripeWebhookSubscriptionUpdated(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	installAuroraPayments(t, &fakePayments{
		events: []aurora.Event{{
			ID:   "evt_sub_updated",
			Type: "customer.subscription.updated",
			Raw: []byte(`{"id":"sub_1","customer":{"id":"cus_1"},"status":"past_due",` +
				`"cancel_at_period_end":true,"items":{"data":[{"current_period_end":4102444800}]}}`),
		}},
	})
	dbfx.Insert(t, "aurora_subscription", testutil.Cols{
		"user_id": testUserID, "tier": "creator", "status": "active",
		"stripe_subscription_id": "sub_1",
	})

	postStripeWebhook(t, `{"type":"customer.subscription.updated"}`, http.StatusOK)

	tier, status, end := auroraSubscriptionRow(t)
	if tier != "creator" || status != "past_due" {
		t.Fatalf("subscription row = %s/%s, want creator/past_due", tier, status)
	}
	if end == nil || !end.UTC().Equal(time.Unix(4102444800, 0).UTC()) {
		t.Fatalf("current_period_end = %v, want the item's period end", end)
	}
	bal, err := testHandler.Credit.Balance(context.Background(), parseUUID(testUserID))
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 0 {
		t.Fatalf("balance = %d after a status update, want 0 (no grant)", bal)
	}
}

// A cancellation stops future grants but does not claw back what was already
// granted — the documented MVP semantic.
func TestStripeWebhookSubscriptionDeleted(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	installAuroraPayments(t, &fakePayments{
		events: []aurora.Event{{
			ID:   "evt_sub_deleted",
			Type: "customer.subscription.deleted",
			Raw:  []byte(`{"id":"sub_1","customer":{"id":"cus_1"},"status":"canceled"}`),
		}},
	})
	dbfx.Insert(t, "aurora_subscription", testutil.Cols{
		"user_id": testUserID, "tier": "creator", "status": "active",
		"stripe_subscription_id": "sub_1",
	})

	postStripeWebhook(t, `{"type":"customer.subscription.deleted"}`, http.StatusOK)

	tier, status, _ := auroraSubscriptionRow(t)
	if tier != "creator" || status != "canceled" {
		t.Fatalf("subscription row = %s/%s, want creator/canceled", tier, status)
	}
}

// Stripe sends far more event types than this product consumes. An unhandled
// one must be acked, not a 4xx that makes Stripe redeliver it forever.
func TestStripeWebhookIgnoresUnknownEventType(t *testing.T) {
	installAuroraPayments(t, &fakePayments{
		events: []aurora.Event{{ID: "evt_invoice", Type: "invoice.paid", Raw: []byte(`{}`)}},
	})
	postStripeWebhook(t, `{"type":"invoice.paid"}`, http.StatusOK)
}

// ---------------------------------------------------------------------------
// Local entitlement gates (Plan 5 Task 6)
// ---------------------------------------------------------------------------

// seedAuroraGenerationForUser inserts one generation row for a user, dated now,
// and removes it when the test ends. Counting rows is how the monthly gate
// measures usage, so a test that needs usage seeds rows rather than running the
// whole create flow.
func seedAuroraGenerationForUser(t *testing.T, userID string) string {
	t.Helper()
	return dbfx.Insert(t, "aurora_generation", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"user_id":      userID,
		"skill_id":     "xhs-image",
		"prompt":       "seeded for an entitlement test",
		"status":       "completed",
	})
}

// The monthly cap is a hard stop: the eleventh generation in a month is refused
// for a free user, before anything is written.
func TestCreateAuroraGenerationRejectsOverMonthlyLimit(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	cleanupAuroraSystemAgents(t)
	free, _ := testHandler.Tiers.Lookup("free")
	for i := 0; i < free.GenerationsPerMonth; i++ {
		seedAuroraGenerationForUser(t, testUserID)
	}

	req := newRequest(http.MethodPost, "/api/aurora/generations", map[string]string{
		"skillId": "xhs-image",
		"prompt":  "超出月度上限的生成请求",
	})
	testutil.Call(t, testHandler.CreateAuroraGeneration, req).Want(http.StatusTooManyRequests)

	// A refused request must not have created a generation row of its own.
	if n := dbfx.Count(t,
		`SELECT count(*) FROM aurora_generation WHERE user_id = $1 AND status = 'queued'`,
		testUserID,
	); n != 0 {
		t.Fatalf("queued generations = %d after a refused request, want 0", n)
	}
}

// Concurrency is the second gate, and it counts in-flight work rather than the
// month's total: a free user may have one generation running at a time.
func TestCreateAuroraGenerationRejectsOverConcurrency(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	cleanupAuroraSystemAgents(t)

	agentID := dbfx.Agent(t, "Aurora concurrency gate agent", testRuntimeID)
	taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "queued", "runtime_id": testRuntimeID})
	seedAuroraGenerationForUser(t, testUserID)
	dbfx.Exec(t, `UPDATE aurora_generation SET task_id = $1 WHERE user_id = $2 AND task_id IS NULL`, taskID, testUserID)

	req := newRequest(http.MethodPost, "/api/aurora/generations", map[string]string{
		"skillId": "xhs-image",
		"prompt":  "超出并发上限的生成请求",
	})
	testutil.Call(t, testHandler.CreateAuroraGeneration, req).Want(http.StatusTooManyRequests)
}

// A free user with an empty wallet gets the month's free credits on their first
// generation of the month — granted before the reservation, or the request
// would be rejected for insufficient credits. A second generation in the same
// month must not grant them again.
func TestCreateAuroraGenerationGrantsFreeMonthlyCreditsOnce(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	cleanupAuroraSystemAgents(t)
	ctx := context.Background()

	// xhs-image costs 620 credits and the free grant is 200, so the first
	// generation still fails for want of credits — what this test pins is that
	// the grant happened, and happened once.
	req := func() *http.Request {
		return newRequest(http.MethodPost, "/api/aurora/generations", map[string]string{
			"skillId": "xhs-image",
			"prompt":  "免费额度发放测试",
		})
	}
	testutil.Call(t, testHandler.CreateAuroraGeneration, req()).Want(http.StatusPaymentRequired)

	bal, err := testHandler.Credit.Balance(ctx, parseUUID(testUserID))
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != testHandler.Tiers.FreeMonthlyMicro() {
		t.Fatalf("balance = %d, want the free monthly grant %d", bal, testHandler.Tiers.FreeMonthlyMicro())
	}

	testutil.Call(t, testHandler.CreateAuroraGeneration, req()).Want(http.StatusPaymentRequired)
	bal2, err := testHandler.Credit.Balance(ctx, parseUUID(testUserID))
	if err != nil {
		t.Fatalf("Balance after the second request: %v", err)
	}
	if bal2 != bal {
		t.Fatalf("the free monthly grant was duplicated: before=%d after=%d", bal, bal2)
	}
}

// A free grant is a monthly grant like any other: the settlement expires its
// unused remainder, which is why the lazy grant uses the same "sub:" reference
// shape as a paid plan.
func TestEnsureFreeMonthlyGrantUsesTheMonthlyReference(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	ctx := context.Background()

	if err := testHandler.ensureFreeMonthlyGrant(ctx, parseUUID(testUserID)); err != nil {
		t.Fatalf("ensureFreeMonthlyGrant: %v", err)
	}
	want := "sub:" + testUserID + ":" + time.Now().UTC().Format("2006-01")
	if n := dbfx.Count(t,
		`SELECT count(*) FROM credit_ledger WHERE user_id = $1 AND kind = 'adjustment' AND reference = $2`,
		testUserID, want,
	); n != 1 {
		t.Fatalf("ledger rows with reference %q = %d, want 1", want, n)
	}
}

// ---------------------------------------------------------------------------
// Subscription status and topup list (Plan 5 Task 7)
// ---------------------------------------------------------------------------

type auroraSubscriptionPayload struct {
	Subscription struct {
		Tier              string  `json:"tier"`
		Status            string  `json:"status"`
		CurrentPeriodEnd  *string `json:"currentPeriodEnd"`
		CancelAtPeriodEnd bool    `json:"cancelAtPeriodEnd"`
		Limits            struct {
			GenerationsPerMonth int `json:"generationsPerMonth"`
			Concurrency         int `json:"concurrency"`
		} `json:"limits"`
		Usage struct {
			GenerationsUsedThisMonth int64 `json:"generationsUsedThisMonth"`
			ActiveGenerations        int64 `json:"activeGenerations"`
		} `json:"usage"`
	} `json:"subscription"`
}

func getAuroraSubscription(t *testing.T) auroraSubscriptionPayload {
	t.Helper()
	req := newRequest(http.MethodGet, "/api/aurora/billing/subscription", nil)
	return testutil.Decode[auroraSubscriptionPayload](t, testHandler.GetAuroraSubscription, req, http.StatusOK)
}

// A user who never subscribed is not an error: the endpoint answers with the
// free tier and no period, so the plan screen renders one shape either way.
func TestAuroraSubscriptionDefaultsToFree(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	free, ok := testHandler.Tiers.Lookup("free")
	if !ok {
		t.Fatal("free tier missing from the catalog")
	}

	out := getAuroraSubscription(t).Subscription
	if out.Tier != "free" {
		t.Fatalf("tier = %q, want free", out.Tier)
	}
	if out.Status != "" {
		t.Fatalf("status = %q, want empty for a user with no subscription row", out.Status)
	}
	if out.CurrentPeriodEnd != nil {
		t.Fatalf("currentPeriodEnd = %v, want null", *out.CurrentPeriodEnd)
	}
	if out.Limits.GenerationsPerMonth != free.GenerationsPerMonth || out.Limits.Concurrency != free.Concurrency {
		t.Fatalf("limits = %+v, want the free tier %+v", out.Limits, free)
	}
}

// The response's tier is the effective one — the tier its limits belong to. A
// canceled creator is on free limits, and reporting "creator" beside them would
// describe a plan the user does not have.
func TestAuroraSubscriptionReportsTheEffectiveTier(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	dbfx.Insert(t, "aurora_subscription", testutil.Cols{
		"user_id": testUserID, "tier": "creator", "status": "canceled",
	})

	out := getAuroraSubscription(t).Subscription
	if out.Tier != "free" {
		t.Fatalf("tier = %q, want free for a canceled plan", out.Tier)
	}
	if out.Status != "canceled" {
		t.Fatalf("status = %q, want canceled", out.Status)
	}
}

func TestAuroraSubscriptionReportsAnActivePlan(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	pro, ok := testHandler.Tiers.Lookup("pro")
	if !ok {
		t.Fatal("pro tier missing from the catalog")
	}
	periodEnd := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	dbfx.Insert(t, "aurora_subscription", testutil.Cols{
		"user_id": testUserID, "tier": "pro", "status": "active",
		"current_period_end":     periodEnd,
		"cancel_at_period_end":   true,
		"stripe_subscription_id": "sub_status_" + uuid.NewString(),
	})

	out := getAuroraSubscription(t).Subscription
	if out.Tier != "pro" || out.Status != "active" {
		t.Fatalf("subscription = %s/%s, want pro/active", out.Tier, out.Status)
	}
	if !out.CancelAtPeriodEnd {
		t.Fatal("cancelAtPeriodEnd = false, want true")
	}
	if out.Limits.GenerationsPerMonth != pro.GenerationsPerMonth || out.Limits.Concurrency != pro.Concurrency {
		t.Fatalf("limits = %+v, want the pro tier %+v", out.Limits, pro)
	}
	if out.CurrentPeriodEnd == nil {
		t.Fatal("currentPeriodEnd = null, want the stored period end")
	}
	parsed, err := time.Parse(time.RFC3339Nano, *out.CurrentPeriodEnd)
	if err != nil {
		t.Fatalf("currentPeriodEnd %q is not RFC3339: %v", *out.CurrentPeriodEnd, err)
	}
	if !parsed.Equal(periodEnd) {
		t.Fatalf("currentPeriodEnd = %v, want %v", parsed, periodEnd)
	}
}

// The plan screen shows this month's usage against the plan's cap, so the
// endpoint reports the same counts the entitlement gate enforces.
func TestAuroraSubscriptionReportsUsage(t *testing.T) {
	creditTestReset(t)
	auroraSubscriptionTestReset(t)
	seedAuroraGenerationForUser(t, testUserID)

	out := getAuroraSubscription(t).Subscription
	if out.Usage.GenerationsUsedThisMonth != 1 {
		t.Fatalf("generationsUsedThisMonth = %d, want 1", out.Usage.GenerationsUsedThisMonth)
	}
	if out.Usage.ActiveGenerations != 0 {
		t.Fatalf("activeGenerations = %d, want 0 (the seeded row has no task)", out.Usage.ActiveGenerations)
	}
}

func TestAuroraSubscriptionRequiresAuthentication(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/aurora/billing/subscription", nil)
	testutil.Call(t, testHandler.GetAuroraSubscription, req).Want(http.StatusUnauthorized)
}

// The topup list is what the plan screen's purchase buttons are built from. The
// Stripe price id stays server-side: it is deployment configuration and the
// client only ever passes the id back.
func TestListAuroraTopups(t *testing.T) {
	req := newRequest(http.MethodGet, "/api/aurora/billing/topups", nil)
	out := testutil.Decode[struct {
		Topups []struct {
			ID      string `json:"id"`
			Credits int64  `json:"credits"`
		} `json:"topups"`
	}](t, testHandler.ListAuroraTopups, req, http.StatusOK)

	if len(out.Topups) != 2 {
		t.Fatalf("topups = %d, want 2", len(out.Topups))
	}
	for _, topup := range out.Topups {
		want, ok := testHandler.Tiers.LookupTopup(topup.ID)
		if !ok {
			t.Fatalf("topup %q is not in the catalog", topup.ID)
		}
		if topup.Credits != want.Credits {
			t.Fatalf("topup %q credits = %d, want %d", topup.ID, topup.Credits, want.Credits)
		}
	}
}

// The response must not carry the price id, whatever a future field is called.
func TestListAuroraTopupsHidesPriceIDs(t *testing.T) {
	oldTiers := testHandler.Tiers
	testHandler.Tiers = aurora.NewTierCatalog("", "", "", "", "price_secret_5", "price_secret_20")
	t.Cleanup(func() { testHandler.Tiers = oldTiers })

	req := newRequest(http.MethodGet, "/api/aurora/billing/topups", nil)
	response := testutil.Call(t, testHandler.ListAuroraTopups, req).Want(http.StatusOK)
	if body := response.Text(); strings.Contains(body, "price_secret") {
		t.Fatalf("topup response leaks a price id: %s", body)
	}
}
