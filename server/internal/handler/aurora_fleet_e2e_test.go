package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/aurorafleet"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestAuroraFleetProvisionToClaim is the fleet acceptance test: a node
// provisioned by the self-host controller bootstraps a sandbox daemon that
// exchanges its scoped enrollment secret (Task 3) for a daemon credential and
// then claims a queued task as the daemon identity it just enrolled. The daemon
// itself is simulated with the server's own HTTP endpoints — the exact calls a
// real daemon inside the node would make — while the node runs on the fleet's
// in-memory backend so the test needs no Docker daemon.
func TestAuroraFleetProvisionToClaim(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	withSandboxEnrollment(t, newSandboxEnrollmentService())
	ctx := context.Background()

	// Clear any managed runtime a previous handler test left in the shared
	// workspace: the one-managed-runtime-per-workspace index makes the seed
	// below collide otherwise.
	dbfx.Exec(t, `DELETE FROM aurora_sandbox_node WHERE workspace_id = $1`, testWorkspaceID)
	dbfx.Exec(t, `DELETE FROM daemon_token WHERE workspace_id = $1`, testWorkspaceID)
	dbfx.Exec(t, `DELETE FROM agent_runtime WHERE workspace_id = $1 AND provider = 'aurora_managed'`, testWorkspaceID)

	// Seed the workspace's managed runtime in the offline state a fresh fleet
	// node starts from, a system agent bound to it, and a queued task.
	runtimeID := dbfx.Runtime(t, "Aurora fleet e2e managed runtime", testutil.Cols{
		"provider":     "aurora_managed",
		"status":       "offline",
		"last_seen_at": nil,
	})
	agentID := dbfx.Agent(t, "Aurora fleet e2e agent", runtimeID, testutil.Cols{"kind": "system"})
	issueID := dbfx.Issue(t, "Aurora fleet e2e issue")
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID,
		"issue_id":   issueID,
		"status":     "queued",
	})
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM daemon_token WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM aurora_sandbox_node WHERE workspace_id = $1`, testWorkspaceID)
	})

	// Provision an empty node. The controller injects the server URL the daemon
	// dials; the enrollment secret itself is delivered out of band by the
	// control plane, not baked into the node's environment.
	backend := aurorafleet.NewMemoryBackend()
	ctrl := aurorafleet.NewController(aurorafleet.Config{
		Backend:      backend,
		SandboxImage: "aurora-sandbox:latest",
		ServerURL:    "http://multica.internal",
	})
	node := testutil.Decode[struct {
		ID string `json:"id"`
	}](t, ctrl.Handler().ServeHTTP,
		testutil.JSONRequest(http.MethodPost, "/api/v1/nodes", map[string]string{"name": "sandbox-0"}), http.StatusCreated)
	if node.ID == "" {
		t.Fatal("provisioned node has no id")
	}
	if env := backend.NodeEnv(node.ID); env[aurorafleet.EnvServerURL] != "http://multica.internal" {
		t.Fatalf("injected server url = %q, want %q", env[aurorafleet.EnvServerURL], "http://multica.internal")
	}

	// The sandbox daemon exchanges the workspace-scoped enrollment secret for
	// an mdt_ credential and learns the runtime and daemon identity it serves.
	issued, err := newSandboxEnrollmentService().Issue(ctx, parseUUID(testWorkspaceID), parseUUID(runtimeID), auroraEnrollmentImageDigest)
	if err != nil {
		t.Fatalf("issue managed enrollment: %v", err)
	}
	enrolled := testutil.Decode[struct {
		DaemonID    string `json:"daemon_id"`
		DaemonToken string `json:"daemon_token"`
		Runtime     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"runtime"`
	}](t, testHandler.ManagedRuntimeEnroll, managedEnrollRequest(issued.Token, nil), http.StatusOK)
	if enrolled.Runtime.ID != runtimeID {
		t.Fatalf("enrolled runtime id = %q, want %q", enrolled.Runtime.ID, runtimeID)
	}
	if enrolled.Runtime.Status != "online" {
		t.Fatalf("enrolled runtime status = %q, want online", enrolled.Runtime.Status)
	}
	if enrolled.DaemonID == "" || enrolled.DaemonToken == "" {
		t.Fatalf("enrollment response missing daemon identity: %+v", enrolled)
	}

	// The daemon claims the queued task under the identity it just enrolled.
	claim := testutil.Decode[batchClaimResponse](t, testHandler.ClaimTasksByRuntime,
		newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{
			"daemon_id":   enrolled.DaemonID,
			"runtime_ids": []string{runtimeID},
			"max_tasks":   1,
		}, testWorkspaceID, enrolled.DaemonID),
		http.StatusOK)
	if len(claim.Tasks) != 1 || claim.Tasks[0].ID != taskID {
		t.Fatalf("claimed tasks = %+v, want task %s", claim.Tasks, taskID)
	}
}
