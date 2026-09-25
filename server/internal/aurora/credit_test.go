package aurora_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/aurora"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// auroraTestPool is the package's fixture pool, skipping the test when no
// database is reachable — the same contract every DB-backed test here follows.
func auroraTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if agentsTestPool == nil {
		t.Skip("database not available")
	}
	return agentsTestPool
}

// newTestCreditService builds a credit service on the package's fixture pool.
// The pool is the transaction beginner: the service only needs Begin, and
// spending a connection per operation is what the production wiring does too.
func newTestCreditService(t *testing.T) (*aurora.CreditService, *pgxpool.Pool) {
	t.Helper()
	pool := auroraTestPool(t)
	return aurora.NewCreditService(db.New(pool), pool), pool
}

// newAuroraTestUser creates a throwaway user for a credit test and removes it,
// with its wallet and ledger, when the test ends. The wallet tables carry no
// foreign key to "user", so deleting the user alone would strand them.
func newAuroraTestUser(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Aurora credit test user", "aurora-credit-"+uuid.NewString()+"@multica.test").Scan(&id); err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM credit_ledger WHERE user_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM credit_balance WHERE user_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM aurora_subscription WHERE user_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, id)
	})
	return mustUUID(t, id)
}

// newAuroraTestWorkspace creates a workspace owned by userID and removes it when
// the test ends. The owner membership is what GetPersonalWorkspaceForUser
// resolves, so a credit write can be attributed to a real space.
func newAuroraTestWorkspace(t *testing.T, pool *pgxpool.Pool, userID pgtype.UUID) pgtype.UUID {
	t.Helper()
	slug := "aurora-credit-" + uuid.NewString()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO workspace (name, slug, issue_prefix) VALUES ($1, $2, $3) RETURNING id`,
		"Aurora credit test workspace", slug, "ACR").Scan(&id); err != nil {
		t.Fatalf("insert test workspace: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, id, userID); err != nil {
		t.Fatalf("insert test membership: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, id)
	})
	return mustUUID(t, id)
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

func creditBalanceOf(t *testing.T, pool *pgxpool.Pool, userID pgtype.UUID) int64 {
	t.Helper()
	var balance int64
	if err := pool.QueryRow(context.Background(),
		`SELECT coalesce((SELECT available_micro FROM credit_balance WHERE user_id = $1), 0)`,
		userID).Scan(&balance); err != nil {
		t.Fatalf("read credit balance: %v", err)
	}
	return balance
}

func TestEnsureMonthlyAllowanceTopsUpToTheHighestTier(t *testing.T) {
	svc, pool := newTestCreditService(t)
	user := newAuroraTestUser(t, pool)
	ws := newAuroraTestWorkspace(t, pool, user)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := svc.EnsureMonthlyAllowance(ctx, user, ws, 200, now); err != nil {
		t.Fatalf("free EnsureMonthlyAllowance: %v", err)
	}
	if err := svc.EnsureMonthlyAllowance(ctx, user, ws, 3000, now); err != nil {
		t.Fatalf("creator EnsureMonthlyAllowance: %v", err)
	}
	// Upgrading in the same month grants only the difference. Granting the full
	// paid amount after the Free allowance would produce 3,200 instead.
	if bal := creditBalanceOf(t, pool, user); bal != 3000 {
		t.Fatalf("balance after same-month upgrade = %d, want 3000", bal)
	}

	if err := svc.EnsureMonthlyAllowance(ctx, user, ws, 3000, now); err != nil {
		t.Fatalf("creator retry: %v", err)
	}
	if bal := creditBalanceOf(t, pool, user); bal != 3000 {
		t.Fatalf("balance after retry = %d, want 3000", bal)
	}
}

func TestEnsureMonthlyAllowanceNeverDowngradesTheMonth(t *testing.T) {
	svc, pool := newTestCreditService(t)
	user := newAuroraTestUser(t, pool)
	ws := newAuroraTestWorkspace(t, pool, user)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := svc.EnsureMonthlyAllowance(ctx, user, ws, 3000, now); err != nil {
		t.Fatalf("creator EnsureMonthlyAllowance: %v", err)
	}
	if err := svc.EnsureMonthlyAllowance(ctx, user, ws, 200, now); err != nil {
		t.Fatalf("late free EnsureMonthlyAllowance: %v", err)
	}
	if bal := creditBalanceOf(t, pool, user); bal != 3000 {
		t.Fatalf("balance after lower allowance = %d, want 3000", bal)
	}
}

func TestExpireIsIdempotent(t *testing.T) {
	svc, pool := newTestCreditService(t)
	user := newAuroraTestUser(t, pool)
	ws := newAuroraTestWorkspace(t, pool, user)
	ctx := context.Background()

	if err := svc.Grant(ctx, user, ws, 1000, aurora.LedgerKindAdjustment, "sub-"+uuid.NewString()); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	reference := uuid.NewString() + ":2026-09"
	if err := svc.Expire(ctx, user, ws, 400, reference); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	// The retry is what a settlement rerun does; the second call must not take
	// another 400.
	if err := svc.Expire(ctx, user, ws, 400, reference); err != nil {
		t.Fatalf("Expire retry: %v", err)
	}
	if bal := creditBalanceOf(t, pool, user); bal != 600 {
		t.Fatalf("balance after idempotent expire = %d, want 600", bal)
	}
}

// Expiring more than the wallet holds is refused rather than driving the
// balance negative: the settlement loop relies on this to skip a user who
// already spent below their grant instead of over-deducting.
func TestExpireRejectsMoreThanBalance(t *testing.T) {
	svc, pool := newTestCreditService(t)
	user := newAuroraTestUser(t, pool)
	ws := newAuroraTestWorkspace(t, pool, user)
	ctx := context.Background()

	if err := svc.Grant(ctx, user, ws, 100, aurora.LedgerKindAdjustment, "sub-"+uuid.NewString()); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := svc.Expire(ctx, user, ws, 400, uuid.NewString()+":2026-09"); !errors.Is(err, aurora.ErrInsufficientCredits) {
		t.Fatalf("Expire over balance = %v, want ErrInsufficientCredits", err)
	}
	if bal := creditBalanceOf(t, pool, user); bal != 100 {
		t.Fatalf("balance = %d after a refused expire, want 100", bal)
	}
}

func TestExpireRejectsNonPositiveAmount(t *testing.T) {
	svc, pool := newTestCreditService(t)
	user := newAuroraTestUser(t, pool)
	ws := newAuroraTestWorkspace(t, pool, user)
	ctx := context.Background()

	// A non-positive amount would invert the sign of the ledger row.
	if err := svc.Expire(ctx, user, ws, 0, uuid.NewString()); err == nil {
		t.Fatal("Expire(0) should fail")
	}
}

// seedCreditLedgerRow writes a ledger row with an explicit timestamp and
// idempotency key, which the service cannot do — it always writes now(). The
// settlement tests need rows from a past window.
func seedCreditLedgerRow(t *testing.T, pool *pgxpool.Pool, userID, workspaceID pgtype.UUID, kind string, amountMicro int64, reference, idempotencyKey string, createdAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO credit_ledger (user_id, workspace_id, kind, amount_micro, balance_after_micro, reference, idempotency_key, created_at)
		VALUES ($1, $2, $3, $4, 0, $5, $6, $7)`,
		userID, workspaceID, kind, amountMicro, reference, idempotencyKey, createdAt); err != nil {
		t.Fatalf("seed credit_ledger: %v", err)
	}
}

// seedCreditBalance sets a wallet's balance directly. The settlement reads the
// ledger for its windows but spends from credit_balance, so a test that seeds
// history has to state the resulting balance too.
func seedCreditBalance(t *testing.T, pool *pgxpool.Pool, userID pgtype.UUID, availableMicro int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO credit_balance (user_id, available_micro) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET available_micro = EXCLUDED.available_micro, updated_at = now()`,
		userID, availableMicro); err != nil {
		t.Fatalf("seed credit_balance: %v", err)
	}
}

// ledgerCountAndSum reports how many rows of one kind a user has and what they
// add up to. Signed: an expiry is negative.
func ledgerCountAndSum(t *testing.T, pool *pgxpool.Pool, userID pgtype.UUID, kind string) (int64, int64) {
	t.Helper()
	var count, sum int64
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*), coalesce(sum(amount_micro), 0) FROM credit_ledger WHERE user_id = $1 AND kind = $2`,
		userID, kind).Scan(&count, &sum); err != nil {
		t.Fatalf("sum credit_ledger kind %s: %v", kind, err)
	}
	return count, sum
}
