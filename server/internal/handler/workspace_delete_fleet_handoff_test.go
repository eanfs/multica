package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// recordingFleetControl is the fake Fleet control plane for the workspace
// teardown handoff. DeleteWorkspaceNode is the destroy intent the handler must
// enqueue while the aurora_sandbox_node row is still present, and it samples the
// row through an independent connection at the moment of the call so the test
// can prove the handoff happened before the teardown removed the row.
type recordingFleetControl struct {
	mu      sync.Mutex
	deleted []string
	owners  []string
	err     error

	// nodeStillPresent is set from the row probe; probeErr records a failed
	// probe so the assertion can report it rather than silently pass.
	nodeStillPresent bool
	probeErr         error
}

func (f *recordingFleetControl) EnsureWorkspaceNode(context.Context, string, aurora.FleetEnsureRequest) (aurora.FleetNode, error) {
	return aurora.FleetNode{}, nil
}

func (f *recordingFleetControl) DeleteWorkspaceNode(_ context.Context, ownerID, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, nodeID)
	f.owners = append(f.owners, ownerID)
	if testPool != nil {
		var count int
		f.probeErr = testPool.QueryRow(context.Background(),
			"SELECT count(*) FROM aurora_sandbox_node WHERE id = $1", nodeID).Scan(&count)
		f.nodeStillPresent = f.probeErr == nil && count == 1
	}
	return nil
}

func (f *recordingFleetControl) snapshot() (deleted, owners []string, present bool, probeErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...), append([]string(nil), f.owners...), f.nodeStillPresent, f.probeErr
}

// seedTeardownSandbox creates a throwaway workspace owned by the fixture user,
// with one managed runtime and one online sandbox node that reached the Fleet.
// It returns the workspace id, the node id, and the runtime id. The handler
// teardown removes all three; the cleanup only covers a test that fails before
// it calls DeleteWorkspace.
func seedTeardownSandbox(t *testing.T) (workspaceID, nodeID, runtimeID string) {
	t.Helper()
	ctx := context.Background()
	workspaceID = dbfx.Workspace(t, "Aurora teardown handoff", "aurora-teardown-"+uuid.NewString())
	dbfx.Member(t, workspaceID, testUserID, "owner")
	runtimeID = dbfx.Runtime(t, "Aurora teardown managed runtime", testutil.Cols{
		"workspace_id": workspaceID,
		"provider":     "aurora_managed",
		"status":       "offline",
		"last_seen_at": nil,
	})
	nodeID = uuid.NewString()
	if _, err := testPool.Exec(ctx, `
INSERT INTO aurora_sandbox_node
    (id, workspace_id, runtime_id, daemon_id, backend_node_id, image_digest, state)
VALUES ($1, $2, $3, $4, $5, $6, 'online')`,
		nodeID, workspaceID, runtimeID, uuid.NewString(), "aurora-sbx-"+nodeID[:12], auroraEnrollmentImageDigest); err != nil {
		t.Fatalf("seed sandbox node: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		testPool.Exec(bg, `DELETE FROM aurora_sandbox_node WHERE workspace_id = $1`, workspaceID)
	})
	return workspaceID, nodeID, runtimeID
}

func deleteWorkspaceOverHTTP(t *testing.T, workspaceID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("DELETE", "/api/workspaces/"+workspaceID, nil)
	req = withURLParam(req, "id", workspaceID)
	testHandler.DeleteWorkspace(w, req)
	return w
}

// TestDeleteWorkspace_HandsSandboxNodesToFleet is the regression behind the
// fleet_nodes workspaceDeleteSettle classification: the workspace teardown must
// hand each of the workspace's live sandbox nodes to the Fleet lifecycle before
// it deletes the aurora_sandbox_node rows. Deleting the row first would strand
// the node's container and its data/secrets volumes because the reaper
// enumerates that row and can never see the node again.
//
// The real SandboxManager is driven over the fake Fleet control, so the delete
// is the production intent path (owner resolved from the workspace's managed
// runtime and the node addressed by the server-issued UUID). A missing or
// misordered handoff leaves the fake's call count at zero; a node the Fleet
// control no longer owns is not observable.
func TestDeleteWorkspace_HandsSandboxNodesToFleet(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	workspaceID, nodeID, _ := seedTeardownSandbox(t)

	fleet := &recordingFleetControl{}
	withSandboxManager(t, aurora.NewSandboxManager(testHandler.Queries, testPool, fleet, auroraEnrollmentImageDigest, nil))

	if w := deleteWorkspaceOverHTTP(t, workspaceID); w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace = %d, want 204: %s", w.Code, w.Body.String())
	}

	deleted, owners, present, probeErr := fleet.snapshot()
	if probeErr != nil {
		t.Fatalf("probe node at handoff time: %v", probeErr)
	}
	if len(deleted) != 1 {
		t.Fatalf("fleet deletes = %d (%v), want exactly 1 for the workspace sandbox node", len(deleted), deleted)
	}
	if deleted[0] != nodeID {
		t.Fatalf("fleet delete address = %q, want the server-issued node UUID %q", deleted[0], nodeID)
	}
	if !present {
		t.Error("the fleet handoff ran after the aurora_sandbox_node row was gone; the reaper loses the node if the row is deleted first")
	}
	if len(owners) != 1 || owners[0] != testUserID {
		t.Fatalf("fleet delete owner = %v, want the managed-runtime owner %q", owners, testUserID)
	}

	// The teardown must still complete the local delete: the handoff adds the
	// Fleet intent, it does not keep the row.
	var nodeCount, runtimeCount, workspaceCount int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM aurora_sandbox_node WHERE id = $1", nodeID).Scan(&nodeCount); err != nil {
		t.Fatalf("count sandbox nodes: %v", err)
	}
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM agent_runtime WHERE workspace_id = $1", workspaceID).Scan(&runtimeCount); err != nil {
		t.Fatalf("count runtimes: %v", err)
	}
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM workspace WHERE id = $1", workspaceID).Scan(&workspaceCount); err != nil {
		t.Fatalf("count workspace: %v", err)
	}
	if nodeCount != 0 || runtimeCount != 0 || workspaceCount != 0 {
		t.Fatalf("teardown leftovers: nodes=%d runtimes=%d workspaces=%d, want 0/0/0", nodeCount, runtimeCount, workspaceCount)
	}
}

// TestDeleteWorkspace_HandoffIsSafeWithoutFleet proves the nil-manager path: a
// deployment with no Fleet integration deletes the workspace and its sandbox
// row without issuing any Fleet call and without panicking. The handoff is only
// real when a manager is configured.
func TestDeleteWorkspace_HandoffIsSafeWithoutFleet(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	workspaceID, nodeID, _ := seedTeardownSandbox(t)

	withSandboxManager(t, nil)

	if w := deleteWorkspaceOverHTTP(t, workspaceID); w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace = %d, want 204: %s", w.Code, w.Body.String())
	}

	var nodeCount int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM aurora_sandbox_node WHERE id = $1", nodeID).Scan(&nodeCount); err != nil {
		t.Fatalf("count sandbox nodes: %v", err)
	}
	if nodeCount != 0 {
		t.Fatalf("sandbox nodes = %d, want 0: the nil-manager teardown still owns the local delete", nodeCount)
	}
}
