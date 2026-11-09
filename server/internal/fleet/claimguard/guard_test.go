package claimguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/fleet/barrier"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	testNamespace      = "wt-claimguard"
	testOtherNamespace = "wt-claimguard-other"
)

// ptr lets a test pass NULL/nullable fixture columns without a second helper
// per type.
func ptr[T any](v T) *T { return &v }

func isolatedClaimPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	return testutil.IsolatedDB(t, dsn)
}

func insertClaimUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var userID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Claim Guard Test", fmt.Sprintf("claim-guard-%d@multica.ai", time.Now().UnixNano()),
	).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return userID
}

func insertClaimWorkspace(t *testing.T, pool *pgxpool.Pool, suffix string) string {
	t.Helper()
	var workspaceID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		"Claim Guard Test", "claim-guard-"+suffix, "temporary claim guard test workspace", "CGT",
	).Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return workspaceID
}

// insertManagedRuntime creates the runtime row the guard reads. managedBy is
// written verbatim so a test can flip a runtime between fleet-managed and
// ordinary without another helper. A nil daemonID stores NULL, which together
// with an empty string covers the two "missing daemon id" shapes.
func insertManagedRuntime(t *testing.T, pool *pgxpool.Pool, workspaceID string, daemonID *string, managedBy string) string {
	t.Helper()
	var runtimeID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at, visibility, managed_by)
		VALUES ($1, $2, $3, 'cloud', 'claim_guard_test', 'online', 'test runtime', '{}'::jsonb, now(), 'private', $4)
		RETURNING id`,
		workspaceID, daemonID, "Claim Guard Runtime", managedBy,
	).Scan(&runtimeID); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	return runtimeID
}

// insertFleetNode writes the persisted Fleet state the barrier validates.
// A nil healthCheckedAt stores NULL, the shape of a node that has never been
// observed. namespace is explicit so a test can place the row outside the
// guard's namespace.
func insertFleetNode(t *testing.T, pool *pgxpool.Pool, ownerID, daemonID, targetState string, daemonReady bool, healthCheckedAt *time.Time, namespace string) string {
	t.Helper()
	var nodeID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO fleet_node (owner_id, namespace, name, daemon_id, image, target_state, daemon_ready, health_checked_at)
		VALUES ($1, $2, $3, $4, 'test-image', $5, $6, $7)
		RETURNING id`,
		ownerID, namespace, "claim-guard-node", daemonID, targetState, daemonReady, healthCheckedAt,
	).Scan(&nodeID); err != nil {
		t.Fatalf("create fleet node: %v", err)
	}
	return nodeID
}

func insertFleetCredential(t *testing.T, pool *pgxpool.Pool, nodeID string, revoked bool) {
	t.Helper()
	var revokedAt any
	if revoked {
		revokedAt = time.Now()
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO fleet_node_credential (node_id, token_hash, revoked_at)
		VALUES ($1, $2, $3)`,
		nodeID, fmt.Sprintf("hash-%d", time.Now().UnixNano()), revokedAt,
	); err != nil {
		t.Fatalf("create fleet credential: %v", err)
	}
}

func newClaimGuardFixture(t *testing.T, managedBy string, daemonID *string) (*pgxpool.Pool, *db.Queries, string, string) {
	t.Helper()
	pool := isolatedClaimPool(t)
	ownerID := insertClaimUser(t, pool)
	workspaceID := insertClaimWorkspace(t, pool, fmt.Sprintf("%d", time.Now().UnixNano()))
	runtimeID := insertManagedRuntime(t, pool, workspaceID, daemonID, managedBy)
	return pool, db.New(pool), runtimeID, ownerID
}

func TestCheckClaimRejectsNodeNotRunning(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", ptr("daemon-1"))
	nodeID := insertFleetNode(t, pool, ownerID, "daemon-1", "starting", true, ptr(time.Now()), testNamespace)
	insertFleetCredential(t, pool, nodeID, false)

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

func TestCheckClaimAllowsReadyNode(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", ptr("daemon-1"))
	nodeID := insertFleetNode(t, pool, ownerID, "daemon-1", "running", true, ptr(time.Now()), testNamespace)
	insertFleetCredential(t, pool, nodeID, false)

	guard := New(testNamespace)
	if err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID)); err != nil {
		t.Fatalf("CheckClaim on ready node: %v", err)
	}
}

func TestCheckClaimRejectsNotReadyNode(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", ptr("daemon-1"))
	nodeID := insertFleetNode(t, pool, ownerID, "daemon-1", "running", false, ptr(time.Now()), testNamespace)
	insertFleetCredential(t, pool, nodeID, false)

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

func TestCheckClaimRejectsRevokedCredential(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", ptr("daemon-1"))
	nodeID := insertFleetNode(t, pool, ownerID, "daemon-1", "running", true, ptr(time.Now()), testNamespace)
	insertFleetCredential(t, pool, nodeID, true)

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

func TestCheckClaimRejectsStaleHealth(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", ptr("daemon-1"))
	nodeID := insertFleetNode(t, pool, ownerID, "daemon-1", "running", true, ptr(time.Now().Add(-time.Minute)), testNamespace)
	insertFleetCredential(t, pool, nodeID, false)

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

func TestCheckClaimRejectsNullHealth(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", ptr("daemon-1"))
	nodeID := insertFleetNode(t, pool, ownerID, "daemon-1", "running", true, nil, testNamespace)
	insertFleetCredential(t, pool, nodeID, false)

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

func TestCheckClaimRejectsMissingNodeRow(t *testing.T) {
	_, q, runtimeID, _ := newClaimGuardFixture(t, "local_fleet", ptr("daemon-1"))

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

func TestCheckClaimRejectsNamespaceMismatch(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", ptr("daemon-1"))
	// The node exists, is healthy, and has a credential, but it belongs to a
	// sibling worktree namespace: this API must not claim through it.
	nodeID := insertFleetNode(t, pool, ownerID, "daemon-1", "running", true, ptr(time.Now()), testOtherNamespace)
	insertFleetCredential(t, pool, nodeID, false)

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

// TestCheckClaimRejectsFleetRuntimeWithoutDaemonID pins the fail-closed rule:
// a runtime already marked local_fleet may not silently fall back to ordinary
// treatment when its daemon id is missing or NULL, because no node lock can be
// named for it.
func TestCheckClaimRejectsFleetRuntimeWithoutDaemonID(t *testing.T) {
	cases := map[string]*string{
		"empty": ptr(""),
		"null":  nil,
	}
	for name, daemonID := range cases {
		t.Run(name, func(t *testing.T) {
			_, q, runtimeID, _ := newClaimGuardFixture(t, "local_fleet", daemonID)

			guard := New(testNamespace)
			err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
			if !errors.Is(err, ErrFleetNodeUnavailable) {
				t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
			}
		})
	}
}

func TestCheckClaimSkipsOrdinaryRuntime(t *testing.T) {
	_, q, runtimeID, _ := newClaimGuardFixture(t, "", ptr("daemon-1"))

	guard := New(testNamespace)
	if err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID)); err != nil {
		t.Fatalf("ordinary runtime must not be checked: %v", err)
	}
}

func TestCheckClaimSkipsOrdinaryRuntimeWithoutDaemonID(t *testing.T) {
	_, q, runtimeID, _ := newClaimGuardFixture(t, "", nil)

	guard := New(testNamespace)
	if err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID)); err != nil {
		t.Fatalf("ordinary runtime without a daemon id must not be checked: %v", err)
	}
}

// TestCheckClaimWaitsForExclusiveNodeLock pins the transaction invariant the
// claim path depends on: CheckClaim takes the node's shared advisory lock on
// the caller's transaction and holds it until that transaction ends. A
// maintenance transaction that already holds the exclusive lock must therefore
// stall the claim (here until its short context expires), and releasing the
// maintenance transaction must let the next claim through.
func TestCheckClaimWaitsForExclusiveNodeLock(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", ptr("daemon-1"))
	nodeID := insertFleetNode(t, pool, ownerID, "daemon-1", "running", true, ptr(time.Now()), testNamespace)
	insertFleetCredential(t, pool, nodeID, false)

	ctx := context.Background()
	key := barrier.Key(testNamespace, "daemon-1")

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin maintenance transaction: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if err := barrier.AcquireExclusive(ctx, holder, key); err != nil {
		t.Fatalf("hold exclusive node lock: %v", err)
	}

	claimCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	claimTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin claim transaction: %v", err)
	}
	defer func() { _ = claimTx.Rollback(context.Background()) }()

	started := time.Now()
	err = New(testNamespace).CheckClaim(claimCtx, q.WithTx(claimTx), util.MustParseUUID(runtimeID))
	if err == nil {
		t.Fatal("CheckClaim passed while another transaction held the exclusive node lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CheckClaim error = %v, want context deadline while waiting for the shared lock", err)
	}
	if waited := time.Since(started); waited < 250*time.Millisecond {
		t.Fatalf("CheckClaim returned after %s; want it to wait on the node lock", waited)
	}

	// Releasing the maintenance transaction must immediately unblock the
	// barrier for the next claim transaction.
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatalf("release maintenance transaction: %v", err)
	}
	releasedTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin released claim transaction: %v", err)
	}
	defer func() { _ = releasedTx.Rollback(context.Background()) }()
	if err := New(testNamespace).CheckClaim(ctx, q.WithTx(releasedTx), util.MustParseUUID(runtimeID)); err != nil {
		t.Fatalf("CheckClaim after the exclusive lock released: %v", err)
	}
}
