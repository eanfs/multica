package claimguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const testNamespace = "wt-claimguard"

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
// ordinary without another helper.
func insertManagedRuntime(t *testing.T, pool *pgxpool.Pool, workspaceID, daemonID, managedBy string) string {
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
// healthCheckedAt is "now" unless a test passes a stale value.
func insertFleetNode(t *testing.T, pool *pgxpool.Pool, ownerID, workspaceID, daemonID, targetState string, daemonReady bool, healthCheckedAt time.Time, namespace string) string {
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

func newClaimGuardFixture(t *testing.T, managedBy, daemonID string) (*pgxpool.Pool, *db.Queries, string, string) {
	t.Helper()
	pool := isolatedClaimPool(t)
	ownerID := insertClaimUser(t, pool)
	workspaceID := insertClaimWorkspace(t, pool, fmt.Sprintf("%d", time.Now().UnixNano()))
	runtimeID := insertManagedRuntime(t, pool, workspaceID, daemonID, managedBy)
	return pool, db.New(pool), runtimeID, ownerID
}

func TestCheckClaimRejectsNodeNotRunning(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", "daemon-1")
	workspaceID := runtimeWorkspace(t, pool, runtimeID)
	nodeID := insertFleetNode(t, pool, ownerID, workspaceID, "daemon-1", "starting", true, time.Now(), testNamespace)
	insertFleetCredential(t, pool, nodeID, false)

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

func TestCheckClaimAllowsReadyNode(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", "daemon-1")
	workspaceID := runtimeWorkspace(t, pool, runtimeID)
	nodeID := insertFleetNode(t, pool, ownerID, workspaceID, "daemon-1", "running", true, time.Now(), testNamespace)
	insertFleetCredential(t, pool, nodeID, false)

	guard := New(testNamespace)
	if err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID)); err != nil {
		t.Fatalf("CheckClaim on ready node: %v", err)
	}
}

func TestCheckClaimRejectsRevokedCredential(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", "daemon-1")
	workspaceID := runtimeWorkspace(t, pool, runtimeID)
	nodeID := insertFleetNode(t, pool, ownerID, workspaceID, "daemon-1", "running", true, time.Now(), testNamespace)
	insertFleetCredential(t, pool, nodeID, true)

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

func TestCheckClaimRejectsStaleHealth(t *testing.T) {
	pool, q, runtimeID, ownerID := newClaimGuardFixture(t, "local_fleet", "daemon-1")
	workspaceID := runtimeWorkspace(t, pool, runtimeID)
	nodeID := insertFleetNode(t, pool, ownerID, workspaceID, "daemon-1", "running", true, time.Now().Add(-time.Minute), testNamespace)
	insertFleetCredential(t, pool, nodeID, false)

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

func TestCheckClaimRejectsMissingNodeRow(t *testing.T) {
	_, q, runtimeID, _ := newClaimGuardFixture(t, "local_fleet", "daemon-1")

	guard := New(testNamespace)
	err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID))
	if !errors.Is(err, ErrFleetNodeUnavailable) {
		t.Fatalf("CheckClaim error = %v, want ErrFleetNodeUnavailable", err)
	}
}

func TestCheckClaimSkipsOrdinaryRuntime(t *testing.T) {
	_, q, runtimeID, _ := newClaimGuardFixture(t, "", "daemon-1")

	guard := New(testNamespace)
	if err := guard.CheckClaim(context.Background(), q, util.MustParseUUID(runtimeID)); err != nil {
		t.Fatalf("ordinary runtime must not be checked: %v", err)
	}
}

func runtimeWorkspace(t *testing.T, pool *pgxpool.Pool, runtimeID string) string {
	t.Helper()
	var workspaceID string
	if err := pool.QueryRow(context.Background(), `SELECT workspace_id FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&workspaceID); err != nil {
		t.Fatalf("read runtime workspace: %v", err)
	}
	return workspaceID
}
