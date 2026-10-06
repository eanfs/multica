package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// auroraRuntimeView is the decoded GET /api/aurora/runtime body. It mirrors the
// handler's response shape so a rename on either side fails the test.
type auroraRuntimeView struct {
	WorkspaceID string `json:"workspaceId"`
	Node        *struct {
		ID          string  `json:"id"`
		Status      string  `json:"status"`
		Ready       bool    `json:"ready"`
		Provider    string  `json:"provider"`
		ErrorCode   *string `json:"errorCode"`
		OperationID *string `json:"operationId"`
		CreatedAt   string  `json:"createdAt"`
	} `json:"node"`
	RuntimeID *string `json:"runtimeId"`
	State     string  `json:"state"`
}

// runtimeViewWorkspace creates a throwaway workspace with one managed runtime.
// The node row is created per case, so a case that wants "no node" simply does
// not insert one.
func runtimeViewWorkspace(t *testing.T) (workspaceID, runtimeID string) {
	t.Helper()
	workspaceID = dbfx.Workspace(t, "Aurora runtime view", "aurora-runtime-view-"+uuid.NewString())
	runtimeID = dbfx.Runtime(t, "Aurora runtime view managed", testutil.Cols{
		"workspace_id": workspaceID,
		"provider":     auroraManagedRuntimeProvider,
		"status":       "offline",
		"last_seen_at": nil,
	})
	return workspaceID, runtimeID
}

// runtimeViewRequest scopes the request to workspaceID. The handler resolves the
// workspace from the header exactly as the membership middleware does in
// production.
func runtimeViewRequest(workspaceID string) *http.Request {
	req := newRequest(http.MethodGet, "/api/aurora/runtime", nil)
	req.Header.Set("X-Workspace-ID", workspaceID)
	return req
}

// insertRuntimeNode writes the workspace's single node row.
func insertRuntimeNode(t *testing.T, workspaceID, runtimeID string, cols testutil.Cols) string {
	t.Helper()
	base := testutil.Cols{
		"id":           uuid.NewString(),
		"workspace_id": workspaceID,
		"runtime_id":   runtimeID,
		"daemon_id":    uuid.NewString(),
		"image_digest": "ghcr.io/eanfs/multica-aurora@sha256:" + strings.Repeat("a", 64),
		"state":        "online",
	}
	for k, v := range cols {
		base[k] = v
	}
	return dbfx.Insert(t, "aurora_sandbox_node", base)
}

func decodeRuntimeView(t *testing.T, workspaceID string) auroraRuntimeView {
	t.Helper()
	return testutil.Decode[auroraRuntimeView](
		t, testHandler.GetAuroraRuntime, runtimeViewRequest(workspaceID), http.StatusOK,
	)
}

func TestGetAuroraRuntime(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	t.Run("unconfigured and null node before any provisioning", func(t *testing.T) {
		workspaceID, runtimeID := runtimeViewWorkspace(t)

		out := decodeRuntimeView(t, workspaceID)

		if out.WorkspaceID != workspaceID {
			t.Fatalf("workspaceId = %q, want %q", out.WorkspaceID, workspaceID)
		}
		if out.State != "unconfigured" {
			t.Fatalf("state = %q, want unconfigured", out.State)
		}
		if out.Node != nil {
			t.Fatalf("node = %#v, want null", out.Node)
		}
		// The managed runtime is seeded lazily but may already exist; here it
		// does, and the view still reports the unconfigured node.
		if out.RuntimeID == nil || *out.RuntimeID != runtimeID {
			t.Fatalf("runtimeId = %v, want %q", out.RuntimeID, runtimeID)
		}
	})

	t.Run("provisioning while the node is starting", func(t *testing.T) {
		workspaceID, runtimeID := runtimeViewWorkspace(t)
		nodeID := insertRuntimeNode(t, workspaceID, runtimeID, testutil.Cols{"state": "starting"})

		out := decodeRuntimeView(t, workspaceID)

		if out.State != "provisioning" {
			t.Fatalf("state = %q, want provisioning", out.State)
		}
		if out.Node == nil {
			t.Fatal("node is null")
		}
		if out.Node.ID != nodeID || out.Node.Status != "starting" || out.Node.Ready {
			t.Fatalf("node = %#v, want id %q starting/not-ready", out.Node, nodeID)
		}
	})

	t.Run("online and ready once the node is online", func(t *testing.T) {
		workspaceID, runtimeID := runtimeViewWorkspace(t)
		insertRuntimeNode(t, workspaceID, runtimeID, testutil.Cols{
			"state":           "online",
			"backend_node_id": "fleet-node-1",
		})

		out := decodeRuntimeView(t, workspaceID)

		if out.State != "online" {
			t.Fatalf("state = %q, want online", out.State)
		}
		if out.Node == nil {
			t.Fatal("node is null")
		}
		// The Fleet node identity is the public handle once it exists.
		if out.Node.ID != "fleet-node-1" {
			t.Fatalf("node id = %q, want fleet-node-1", out.Node.ID)
		}
		if !out.Node.Ready || out.Node.Provider != "docker" {
			t.Fatalf("node = %#v, want ready docker", out.Node)
		}
		if out.Node.OperationID != nil {
			t.Fatalf("operationId = %v, want null", *out.Node.OperationID)
		}
		if out.Node.CreatedAt == "" {
			t.Fatal("createdAt is empty")
		}
	})

	t.Run("draining still reads as online", func(t *testing.T) {
		workspaceID, runtimeID := runtimeViewWorkspace(t)
		insertRuntimeNode(t, workspaceID, runtimeID, testutil.Cols{"state": "draining"})

		if out := decodeRuntimeView(t, workspaceID); out.State != "online" {
			t.Fatalf("state = %q, want online", out.State)
		}
	})

	t.Run("stopped reads as unconfigured", func(t *testing.T) {
		workspaceID, runtimeID := runtimeViewWorkspace(t)
		insertRuntimeNode(t, workspaceID, runtimeID, testutil.Cols{"state": "stopped"})

		out := decodeRuntimeView(t, workspaceID)

		if out.State != "unconfigured" {
			t.Fatalf("state = %q, want unconfigured", out.State)
		}
		if out.Node == nil || out.Node.Status != "stopped" {
			t.Fatalf("node = %#v, want stopped status", out.Node)
		}
	})

	t.Run("failed maps the raw reason onto a public code", func(t *testing.T) {
		workspaceID, runtimeID := runtimeViewWorkspace(t)
		reason := "ensure workspace sandbox: dial tcp 10.0.0.5:9000: connect: connection refused"
		insertRuntimeNode(t, workspaceID, runtimeID, testutil.Cols{
			"state":          "failed",
			"failure_reason": reason,
		})

		resp := testutil.Call(t, testHandler.GetAuroraRuntime, runtimeViewRequest(workspaceID)).Want(http.StatusOK)
		var out auroraRuntimeView
		resp.JSON(&out)

		if out.State != "failed" {
			t.Fatalf("state = %q, want failed", out.State)
		}
		if out.Node == nil || out.Node.ErrorCode == nil || *out.Node.ErrorCode != "unavailable" {
			t.Fatalf("errorCode = %#v, want unavailable", out.Node)
		}
		// The raw reason names internal addresses; it must not cross the API.
		body := resp.Text()
		if strings.Contains(body, "10.0.0.5") || strings.Contains(body, "connection refused") {
			t.Fatalf("response leaked the raw failure reason: %s", body)
		}
	})

	t.Run("failed classifies a missing profile", func(t *testing.T) {
		workspaceID, runtimeID := runtimeViewWorkspace(t)
		insertRuntimeNode(t, workspaceID, runtimeID, testutil.Cols{
			"state":          "failed",
			"failure_reason": "provider credential profile is missing",
		})

		out := decodeRuntimeView(t, workspaceID)
		if out.Node == nil || out.Node.ErrorCode == nil || *out.Node.ErrorCode != "profile_missing" {
			t.Fatalf("errorCode = %#v, want profile_missing", out.Node)
		}
	})

	t.Run("another workspace's node stays invisible", func(t *testing.T) {
		otherWorkspace, otherRuntime := runtimeViewWorkspace(t)
		insertRuntimeNode(t, otherWorkspace, otherRuntime, testutil.Cols{"state": "online"})

		workspaceID, _ := runtimeViewWorkspace(t)
		out := decodeRuntimeView(t, workspaceID)

		if out.State != "unconfigured" || out.Node != nil {
			t.Fatalf("foreign node leaked into view: %#v", out)
		}
	})

	t.Run("rejects a malformed workspace id", func(t *testing.T) {
		req := newRequest(http.MethodGet, "/api/aurora/runtime", nil)
		req.Header.Set("X-Workspace-ID", "not-a-uuid")

		testutil.Call(t, testHandler.GetAuroraRuntime, req).Want(http.StatusBadRequest)
	})
}

// TestAuroraRuntimeViewIsReadOnly pins that the view does not mutate the node:
// a read that rewrote last_active_at would make opening the screen a heartbeat
// and hide an idle node from the reaper.
func TestAuroraRuntimeViewIsReadOnly(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	workspaceID, runtimeID := runtimeViewWorkspace(t)
	nodeID := insertRuntimeNode(t, workspaceID, runtimeID, testutil.Cols{"state": "online"})

	const readNodeSQL = "SELECT state, COALESCE(backend_node_id, '') FROM aurora_sandbox_node WHERE id = $1"

	var beforeState, beforeBackend string
	if err := testPool.QueryRow(context.Background(), readNodeSQL, nodeID).Scan(&beforeState, &beforeBackend); err != nil {
		t.Fatalf("read node before: %v", err)
	}

	decodeRuntimeView(t, workspaceID)

	var afterState, afterBackend string
	if err := testPool.QueryRow(context.Background(), readNodeSQL, nodeID).Scan(&afterState, &afterBackend); err != nil {
		t.Fatalf("read node after: %v", err)
	}
	if beforeState != afterState || beforeBackend != afterBackend {
		t.Fatalf("view mutated node: %q/%q -> %q/%q", beforeState, beforeBackend, afterState, afterBackend)
	}
}
