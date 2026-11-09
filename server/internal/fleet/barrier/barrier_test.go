package barrier

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	return testutil.IsolatedDB(t, dsn)
}

func TestKeyIsStableAndNamespaceScoped(t *testing.T) {
	a := Key("wt-1", "node-1")
	if a != Key("wt-1", "node-1") {
		t.Fatal("key must be stable")
	}
	if a == Key("wt-2", "node-1") || a == Key("wt-1", "node-2") {
		t.Fatal("key must include namespace and node id")
	}
}

func TestKeysDedupesAndSorts(t *testing.T) {
	// Hashed keys are not in lexicographic node order: for these ids
	// Key("wt", "a") > Key("wt", "b"), so the deduplicated input order is
	// already descending and an implementation that skips the sort returns
	// the keys reversed.
	got := Keys("wt", []string{"a", "b", "a"})
	if len(got) != 2 {
		t.Fatalf("got %d keys (%v), want 2 after deduplication", len(got), got)
	}
	if got[0] != Key("wt", "b") || got[1] != Key("wt", "a") {
		t.Fatalf("got %v, want ascending [%d %d]", got, Key("wt", "b"), Key("wt", "a"))
	}
}

func TestSharedLocksDoNotConflict(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback(ctx)
	if err := AcquireShared(ctx, tx1, Key("wt", "n")); err != nil {
		t.Fatal(err)
	}

	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(ctx)
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := AcquireShared(lockCtx, tx2, Key("wt", "n")); err != nil {
		t.Fatalf("shared lock must not block shared lock: %v", err)
	}
}

func TestExclusiveLockBlocksUntilHolderReleases(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	key := Key("wt", "n")

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Rollback is idempotent; the happy path below rolls back explicitly.
	defer tx1.Rollback(ctx)
	if err := AcquireExclusive(ctx, tx1, key); err != nil {
		t.Fatal(err)
	}

	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(ctx)
	lockCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := AcquireShared(lockCtx, tx2, key); err == nil {
		t.Fatal("shared lock must block while exclusive holder exists")
	}
	// The cancelled query leaves tx2 aborted, so release both transactions and
	// use a fresh one for the post-release check.
	if err := tx1.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	tx3, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx3.Rollback(ctx)
	if err := AcquireShared(ctx, tx3, key); err != nil {
		t.Fatalf("lock after release: %v", err)
	}
}
