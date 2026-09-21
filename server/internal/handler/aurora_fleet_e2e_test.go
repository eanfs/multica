package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/aurorafleet"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestAuroraFleetProvisionToClaim is the Task 5 acceptance test: a node
// provisioned by the fleet controller bootstraps a sandbox daemon that
// registers as managed (Task 3) and claims a queued task (Task 3's unbound
// cloud-runtime claim path). The daemon itself is simulated with the server's
// own HTTP endpoints — the exact calls a real daemon inside the node would make
// — while the node runs on the fleet's in-memory backend so the test needs no
// Docker daemon.
func TestAuroraFleetProvisionToClaim(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	const token = "aurora-fleet-e2e-sandbox-token"
	setTestSandboxToken(t, token)

	// Seed the workspace's managed runtime in the offline/unseen state
	// EnsureSystemAgents leaves it in, a system agent bound to it, and a queued
	// task the daemon should claim.
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

	// Provision an empty node. The controller injects the server URL and the
	// managed-registration token into the node's environment.
	backend := aurorafleet.NewMemoryBackend()
	ctrl := aurorafleet.NewController(aurorafleet.Config{
		Backend:      backend,
		SandboxImage: "aurora-sandbox:latest",
		ServerURL:    "http://multica.internal",
		SandboxToken: token,
	})

	node := testutil.Decode[struct {
		ID string `json:"id"`
	}](t, ctrl.Handler().ServeHTTP,
		testutil.JSONRequest(http.MethodPost, "/api/v1/nodes", map[string]string{"name": "sandbox-0"}), http.StatusCreated)
	if node.ID == "" {
		t.Fatal("provisioned node has no id")
	}

	// The fleet secret reached the node's environment.
	env := backend.NodeEnv(node.ID)
	if env[aurorafleet.EnvSandboxToken] != token {
		t.Fatalf("injected sandbox token = %q, want %q", env[aurorafleet.EnvSandboxToken], token)
	}

	// The sandbox daemon registers as managed, using the injected token.
	register := testutil.Decode[struct {
		Runtime struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"runtime"`
	}](t, testHandler.ManagedRuntimeRegister,
		managedRegisterRequest(token, testWorkspaceID), http.StatusOK)
	if register.Runtime.ID != runtimeID {
		t.Fatalf("registered runtime id = %q, want %q", register.Runtime.ID, runtimeID)
	}
	if register.Runtime.Status != "online" {
		t.Fatalf("registered runtime status = %q, want online", register.Runtime.Status)
	}

	// The daemon then claims the queued task for the runtime it just registered.
	w := postBatchClaim(t, testWorkspaceID, []string{runtimeID}, 1)
	if w.Code != http.StatusOK {
		t.Fatalf("claim status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var claim batchClaimResponse
	if err := json.Unmarshal(w.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if len(claim.Tasks) != 1 || claim.Tasks[0].ID != taskID {
		t.Fatalf("claimed tasks = %+v, want task %s", claim.Tasks, taskID)
	}
}
