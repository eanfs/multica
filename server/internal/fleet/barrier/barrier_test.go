package barrier

import (
	"context"
	"os"
	"sort"
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
	got := Keys("wt", []string{"b", "a", "b"})
	// Hashed keys are not in lexicographic node order, so sort the expectation
	// instead of assuming Key("a") < Key("b").
	want := []int64{Key("wt", "a"), Key("wt", "b")}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
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
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := AcquireExclusive(ctx, tx1, Key("wt", "n")); err != nil {
		t.Fatal(err)
	}

	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(ctx)
	lockCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := AcquireShared(lockCtx, tx2, Key("wt", "n")); err == nil {
		t.Fatal("shared lock must block while exclusive holder exists")
	}
	if err := tx1.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := AcquireShared(ctx, tx2, Key("wt", "n")); err != nil {
		t.Fatalf("lock after release: %v", err)
	}
}
