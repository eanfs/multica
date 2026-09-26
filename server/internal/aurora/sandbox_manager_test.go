package aurora_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/aurorafleet"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeFleet is the manager's view of the fleet control API without HTTP. It
// records every ensure request and answers with a configured node or error, so
// the manager's ordering and rollback can be asserted directly.
type fakeFleet struct {
	mu    sync.Mutex
	calls int
	last  aurorafleet.EnsureRequest
	node  aurorafleet.Node
	err   error
}

func (f *fakeFleet) EnsureWorkspaceNode(_ context.Context, req aurorafleet.EnsureRequest) (aurorafleet.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = req
	if f.err != nil {
		return aurorafleet.Node{}, f.err
	}
	node := f.node
	if node.ID == "" {
		node = aurorafleet.Node{ID: "fleet-" + req.NodeID, State: aurorafleet.StateStarting}
	}
	return node, nil
}

func (f *fakeFleet) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestSandboxManagerReturnsHealthyOnlineNodeWithoutFleetCall pins the no-op
// path: an online node whose runtime and image still match is already the
// desired state, so Ensure must not disturb it or call the fleet.
func TestSandboxManagerReturnsHealthyOnlineNodeWithoutFleetCall(t *testing.T) {
	pool := auroraTestPool(t)
	q := db.New(pool)
	ws, runtimeID := newSandboxNodeWorkspace(t, q, pool)
	ctx := context.Background()

	params := sandboxNodeParams(t, ws, runtimeID, "aurora-"+uuid.NewString())
	params.ImageDigest = validSandboxImageDigest
	created, err := q.CreateAuroraSandboxNode(ctx, params)
	if err != nil {
		t.Fatalf("create starting node: %v", err)
	}
	if _, err := q.ConsumeAuroraSandboxEnrollment(ctx, params.EnrollmentTokenHash); err != nil {
		t.Fatalf("consume enrollment to online: %v", err)
	}

	fleet := &fakeFleet{}
	mgr := aurora.NewSandboxManager(q, pool, fleet, validSandboxImageDigest, nil)
	node, err := mgr.Ensure(ctx, ws, runtimeID)
	if err != nil {
		t.Fatalf("ensure healthy node: %v", err)
	}
	if node.ID != created.ID {
		t.Fatalf("ensured node id = %v, want %v", node.ID, created.ID)
	}
	if node.State != "online" {
		t.Fatalf("ensured node state = %q, want online", node.State)
	}
	if fleet.callCount() != 0 {
		t.Fatalf("fleet ensure calls = %d, want 0 for a healthy online node", fleet.callCount())
	}
	if node.EnrollmentTokenHash.Valid {
		t.Fatal("returned node carries an enrollment token hash")
	}
}

// TestSandboxManagerIssuesEnrollmentAndCallsFleetOnce pins first provision:
// Ensure mints one single-use enrollment, sends exactly one fleet request
// carrying that secret and the node identity, and persists the backend id the
// fleet returns.
func TestSandboxManagerIssuesEnrollmentAndCallsFleetOnce(t *testing.T) {
	pool := auroraTestPool(t)
	q := db.New(pool)
	ws, runtimeID := newSandboxNodeWorkspace(t, q, pool)
	ctx := context.Background()

	fleet := &fakeFleet{node: aurorafleet.Node{ID: "backend-node-7", State: aurorafleet.StateStarting}}
	mgr := aurora.NewSandboxManager(q, pool, fleet, validSandboxImageDigest, nil)
	node, err := mgr.Ensure(ctx, ws, runtimeID)
	if err != nil {
		t.Fatalf("ensure new node: %v", err)
	}
	if node.State != "starting" {
		t.Fatalf("ensured node state = %q, want starting (the daemon has not enrolled yet)", node.State)
	}
	if fleet.callCount() != 1 {
		t.Fatalf("fleet ensure calls = %d, want 1", fleet.callCount())
	}
	if got, want := fleet.last.NodeID, util.UUIDToString(node.ID); got != want {
		t.Fatalf("fleet node id = %q, want %q", got, want)
	}
	if got, want := fleet.last.WorkspaceID, util.UUIDToString(ws); got != want {
		t.Fatalf("fleet workspace id = %q, want %q", got, want)
	}
	if got, want := fleet.last.RuntimeID, util.UUIDToString(runtimeID); got != want {
		t.Fatalf("fleet runtime id = %q, want %q", got, want)
	}
	if fleet.last.DaemonID != node.DaemonID {
		t.Fatalf("fleet daemon id = %q, want %q", fleet.last.DaemonID, node.DaemonID)
	}
	if !strings.HasPrefix(fleet.last.EnrollmentToken, "mse_") {
		t.Fatalf("fleet enrollment token %q is not a single-use mse_ secret", fleet.last.EnrollmentToken)
	}

	stored := sandboxNodeByWorkspace(t, pool, ws)
	if stored.State != "starting" {
		t.Fatalf("stored node state = %q, want starting", stored.State)
	}
	if !stored.BackendNodeID.Valid || stored.BackendNodeID.String != "backend-node-7" {
		t.Fatalf("stored backend node id = %+v, want backend-node-7", stored.BackendNodeID)
	}
	if !stored.EnrollmentTokenHash.Valid || stored.EnrollmentTokenHash.String != auth.HashToken(fleet.last.EnrollmentToken) {
		t.Fatalf("stored enrollment hash = %+v, want the shipped token's hash", stored.EnrollmentTokenHash)
	}
}

// TestSandboxManagerConcurrentEnsureCreatesOneNode proves the workspace
// advisory lock serialises first provision: racing callers converge on one node
// row and exactly one fleet ensure request, and later callers adopt the node
// the winner armed instead of minting a competing secret.
func TestSandboxManagerConcurrentEnsureCreatesOneNode(t *testing.T) {
	pool := auroraTestPool(t)
	q := db.New(pool)
	ws, runtimeID := newSandboxNodeWorkspace(t, q, pool)
	ctx := context.Background()

	fleet := &fakeFleet{}
	mgr := aurora.NewSandboxManager(q, pool, fleet, validSandboxImageDigest, nil)

	const workers = 8
	var wg sync.WaitGroup
	nodes := make([]db.AuroraSandboxNode, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			nodes[i], errs[i] = mgr.Ensure(ctx, ws, runtimeID)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	for i := 1; i < workers; i++ {
		if nodes[i].ID != nodes[0].ID {
			t.Fatalf("ensure %d node id = %v, want %v", i, nodes[i].ID, nodes[0].ID)
		}
	}
	if fleet.callCount() != 1 {
		t.Fatalf("fleet ensure calls = %d, want exactly 1", fleet.callCount())
	}
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM aurora_sandbox_node WHERE workspace_id = $1", ws).Scan(&rows); err != nil {
		t.Fatalf("count sandbox nodes: %v", err)
	}
	if rows != 1 {
		t.Fatalf("sandbox nodes = %d, want exactly 1", rows)
	}
}

// TestSandboxManagerMarksNodeFailedWhenFleetRejects pins rollback: a failed
// fleet ensure leaves no live enrollment and records the node as failed with
// the backend's error.
func TestSandboxManagerMarksNodeFailedWhenFleetRejects(t *testing.T) {
	pool := auroraTestPool(t)
	q := db.New(pool)
	ws, runtimeID := newSandboxNodeWorkspace(t, q, pool)
	ctx := context.Background()

	fleet := &fakeFleet{err: errors.New("fleet exploded")}
	mgr := aurora.NewSandboxManager(q, pool, fleet, validSandboxImageDigest, nil)
	if _, err := mgr.Ensure(ctx, ws, runtimeID); err == nil {
		t.Fatal("ensure succeeded, want the fleet error")
	}

	stored := sandboxNodeByWorkspace(t, pool, ws)
	if stored.State != "failed" {
		t.Fatalf("stored node state = %q, want failed", stored.State)
	}
	if !stored.FailureReason.Valid || !strings.Contains(stored.FailureReason.String, "fleet exploded") {
		t.Fatalf("stored failure reason = %+v, want the fleet error", stored.FailureReason)
	}
	if stored.EnrollmentTokenHash.Valid || stored.EnrollmentExpiresAt.Valid || stored.EnrollmentConsumedAt.Valid {
		t.Fatalf("failed node kept enrollment fields: hash=%+v expires=%+v consumed=%+v",
			stored.EnrollmentTokenHash, stored.EnrollmentExpiresAt, stored.EnrollmentConsumedAt)
	}
	if stored.BackendNodeID.Valid {
		t.Fatalf("failed node kept backend node id %q", stored.BackendNodeID.String)
	}
}

// TestSandboxManagerDoesNotReturnEnrollmentToken pins the secret boundary: the
// raw enrollment secret is handed to the fleet only and never leaks through the
// value Ensure returns, while the database still holds its hash.
func TestSandboxManagerDoesNotReturnEnrollmentToken(t *testing.T) {
	pool := auroraTestPool(t)
	q := db.New(pool)
	ws, runtimeID := newSandboxNodeWorkspace(t, q, pool)
	ctx := context.Background()

	fleet := &fakeFleet{}
	mgr := aurora.NewSandboxManager(q, pool, fleet, validSandboxImageDigest, nil)
	node, err := mgr.Ensure(ctx, ws, runtimeID)
	if err != nil {
		t.Fatalf("ensure new node: %v", err)
	}
	if node.EnrollmentTokenHash.Valid {
		t.Fatalf("returned node carries enrollment hash %q", node.EnrollmentTokenHash.String)
	}
	if node.EnrollmentExpiresAt.Valid {
		t.Fatalf("returned node carries enrollment expiry %v", node.EnrollmentExpiresAt.Time)
	}

	stored := sandboxNodeByWorkspace(t, pool, ws)
	if !stored.EnrollmentTokenHash.Valid || !stored.EnrollmentExpiresAt.Valid {
		t.Fatal("Ensure returned before issuing a live enrollment")
	}
}
