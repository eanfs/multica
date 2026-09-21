package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// setTestSandboxToken pins the shared Aurora sandbox token for a single test and
// restores the previous value afterwards, so no test leaves the suite-level
// handler configured against another test's secret.
func setTestSandboxToken(t *testing.T, token string) {
	t.Helper()
	prev := testHandler.cfg.AuroraSandboxToken
	testHandler.cfg.AuroraSandboxToken = token
	t.Cleanup(func() { testHandler.cfg.AuroraSandboxToken = prev })
}

// TestManagedRuntimeClaimsUnboundCloudRuntime pins the claim-path tolerance the
// managed runtime rides on: a cloud runtime with a NULL daemon_id (the shape
// EnsureSystemAgents seeds) is not machine-pinned, so a machine-level daemon
// claim that names it hands over the queued system-agent task. This is the
// mechanism a sandbox daemon uses to pick up Aurora work without the runtime
// being bound to any one machine.
func TestManagedRuntimeClaimsUnboundCloudRuntime(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := dbfx.Runtime(t, "Aurora unbound managed runtime")
	agentID := dbfx.Agent(t, "Aurora managed claim agent", runtimeID, testutil.Cols{
		"kind": "system",
	})
	issueID := dbfx.Issue(t, "Aurora managed claim issue")
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID,
		"issue_id":   issueID,
		"status":     "queued",
	})

	w := postBatchClaim(t, testWorkspaceID, []string{runtimeID}, 1)
	if w.Code != http.StatusOK {
		t.Fatalf("claim: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp batchClaimResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	if len(resp.Tasks) != 1 || resp.Tasks[0].ID != taskID {
		t.Fatalf("claimed tasks = %+v, want task %s", resp.Tasks, taskID)
	}
}

func TestManagedRuntimeRegisterRejectsWrongToken(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setTestSandboxToken(t, "correct-sandbox-token")

	for _, presented := range []string{"", "wrong-sandbox-token"} {
		req := newRequest(http.MethodPost, "/api/daemon/managed/register", map[string]string{
			"workspace_id": testWorkspaceID,
		})
		if presented != "" {
			req.Header.Set("Authorization", "Bearer "+presented)
		}
		testutil.Call(t, testHandler.ManagedRuntimeRegister, req).Want(http.StatusUnauthorized)
	}
}

func TestManagedRuntimeRegisterRejectsWhenUnconfigured(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setTestSandboxToken(t, "")

	req := newRequest(http.MethodPost, "/api/daemon/managed/register", map[string]string{
		"workspace_id": testWorkspaceID,
	})
	req.Header.Set("Authorization", "Bearer anything")
	testutil.Call(t, testHandler.ManagedRuntimeRegister, req).Want(http.StatusForbidden)
}

func TestManagedRuntimeRegisterRejectsMalformedBody(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setTestSandboxToken(t, "correct-sandbox-token")

	req := httptest.NewRequest(http.MethodPost, "/api/daemon/managed/register", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer correct-sandbox-token")
	testutil.Call(t, testHandler.ManagedRuntimeRegister, req).Want(http.StatusBadRequest)
}

func TestManagedRuntimeRegisterRejectsMalformedWorkspaceID(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setTestSandboxToken(t, "correct-sandbox-token")

	req := newRequest(http.MethodPost, "/api/daemon/managed/register", map[string]string{
		"workspace_id": "not-a-uuid",
	})
	req.Header.Set("Authorization", "Bearer correct-sandbox-token")
	testutil.Call(t, testHandler.ManagedRuntimeRegister, req).Want(http.StatusBadRequest)
}

func TestManagedRuntimeRegisterMarksManagedRuntimeOnline(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setTestSandboxToken(t, "correct-sandbox-token")

	// Seed the managed runtime in the offline/unseen state EnsureSystemAgents
	// leaves it in, so the test proves registration performs the online+fresh
	// transition the claim admission gates require.
	runtimeID := dbfx.Runtime(t, "Aurora managed runtime", testutil.Cols{
		"provider":     aurora.ManagedRuntimeProvider,
		"status":       "offline",
		"last_seen_at": nil,
	})

	req := newRequest(http.MethodPost, "/api/daemon/managed/register", map[string]string{
		"workspace_id": testWorkspaceID,
	})
	req.Header.Set("Authorization", "Bearer correct-sandbox-token")

	out := testutil.Decode[struct {
		Runtime struct {
			ID          string `json:"id"`
			RuntimeMode string `json:"runtime_mode"`
			Status      string `json:"status"`
		} `json:"runtime"`
	}](t, testHandler.ManagedRuntimeRegister, req, http.StatusOK)

	if out.Runtime.ID != runtimeID {
		t.Fatalf("response runtime id = %q, want %q", out.Runtime.ID, runtimeID)
	}
	if out.Runtime.Status != "online" || out.Runtime.RuntimeMode != "cloud" {
		t.Fatalf("response runtime = %+v, want cloud/online", out.Runtime)
	}

	// The registration's persistent mark: status flipped offline→online and
	// last_seen_at refreshed, which is what makes the runtime claimable.
	var status string
	var fresh bool
	dbfx.QueryRow(t, `
		SELECT status, last_seen_at > now() - interval '30 seconds'
		FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&status, &fresh)
	if status != "online" {
		t.Fatalf("runtime status = %q, want online", status)
	}
	if !fresh {
		t.Fatal("registration did not refresh the runtime's last_seen_at")
	}
}
