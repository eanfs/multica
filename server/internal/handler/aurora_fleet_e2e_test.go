package handler

import (
	"context"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/aurorafleet"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

	// Ensure the workspace node through the authenticated internal API. The
	// request carries identity only; the controller stages the enrollment
	// secret into a 0400 host file and hands the backend its path.
	tokenRaw := []byte(strings.Repeat("m", 32))
	tokenFile := filepath.Join(t.TempDir(), "control-token")
	if err := os.WriteFile(tokenFile, []byte(hex.EncodeToString(tokenRaw)), 0o400); err != nil {
		t.Fatalf("write control token file: %v", err)
	}
	auth, err := aurorafleet.LoadControlAuth(tokenFile)
	if err != nil {
		t.Fatalf("load control auth: %v", err)
	}
	backend := aurorafleet.NewMemoryBackend()
	ctrl := aurorafleet.NewController(aurorafleet.Config{Backend: backend, Auth: auth, SecretRoot: t.TempDir()})
	nodeID := "01933e60-0000-7d4e-9f01-2a3b4c5d6e01"
	node := testutil.Decode[struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}](t, ctrl.Handler().ServeHTTP,
		testutil.WithHeaders(
			testutil.JSONRequest(http.MethodPut, "/internal/v1/workspace-nodes/"+nodeID, aurorafleet.EnsureRequest{
				NodeID:          nodeID,
				WorkspaceID:     testWorkspaceID,
				RuntimeID:       runtimeID,
				DaemonID:        "01933e60-0000-7d4e-9f01-2a3b4c5d6e02",
				EnrollmentToken: "mse_0123456789abcdef0123456789abcdef01234567",
			}),
			"Authorization", "Bearer "+string(tokenRaw),
		), http.StatusOK)
	if node.ID != nodeID || node.State != aurorafleet.StateOnline {
		t.Fatalf("ensured node = %+v, want %s online", node, nodeID)
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

	// Every available skill route is materialized on the fleet's managed
	// runtime: the enrolled daemon's claim set therefore reaches each skill's
	// system agent, not just the generic task above. This is the fleet-level
	// half of "prove all 13 routes" — the handler matrix owns the lifecycle.
	if err := aurora.EnsureSystemAgents(ctx, testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(testUserID)); err != nil {
		t.Fatalf("seed aurora system agents: %v", err)
	}
	names := make([]string, 0, len(aurora.Catalog()))
	for _, entry := range aurora.Catalog() {
		names = append(names, entry.Name)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1 AND system_key LIKE 'aurora:%')`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM agent_skill WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1 AND system_key LIKE 'aurora:%')`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM agent WHERE workspace_id = $1 AND system_key LIKE 'aurora:%'`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM skill WHERE workspace_id = $1 AND name = ANY($2::text[])`, testWorkspaceID, names)
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE workspace_id = $1 AND provider = 'aurora_managed'`, testWorkspaceID)
	})
	available := 0
	for _, entry := range aurora.Catalog() {
		if !entry.Available {
			continue
		}
		available++
		systemAgent, err := testHandler.Queries.GetAgentBySystemKey(ctx, db.GetAgentBySystemKeyParams{
			WorkspaceID: parseUUID(testWorkspaceID),
			SystemKey:   pgtype.Text{String: "aurora:" + entry.ID, Valid: true},
		})
		if err != nil {
			t.Fatalf("load %s system agent: %v", entry.ID, err)
		}
		if systemAgent.RuntimeID != parseUUID(runtimeID) {
			t.Fatalf("%s system agent runtime = %s, want the fleet runtime %s", entry.ID, uuidToString(systemAgent.RuntimeID), runtimeID)
		}
		if _, ok := aurora.Workflow(entry.ID); !ok {
			t.Fatalf("skill %s has no reviewed workflow", entry.ID)
		}
	}
	if available != 13 {
		t.Fatalf("available skills = %d, want 13", available)
	}
}
