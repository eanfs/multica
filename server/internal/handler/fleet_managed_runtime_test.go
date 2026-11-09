package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fleetManagedRuntimeFixture inserts a runtime already owned by a local fleet
// node. There is no dbfx.AgentRuntime builder, and managed_by/fleet_node_id are
// server-written columns, so the fixture inserts the row with direct SQL rather
// than routing it through a handler. testUserID/testWorkspaceID are the
// direct-SQL user/workspace fixtures created by TestMain.
func fleetManagedRuntimeFixture(t *testing.T) string {
	t.Helper()
	if testPool == nil {
		t.Skip("database not available")
	}
	var runtimeID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider, status,
			device_info, metadata, owner_id, managed_by
		)
		VALUES ($1, 'daemon-managed', 'claude', 'local', 'claude', 'online',
			'node', '{}'::jsonb, $2, 'local_fleet')
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&runtimeID); err != nil {
		t.Fatalf("insert fleet-managed runtime fixture: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM agent WHERE runtime_id = $1`, runtimeID)
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})
	return runtimeID
}

func assertFleetManagedDeleteRefusal(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Code != "runtime_managed_by_fleet" {
		t.Fatalf("code = %q, want runtime_managed_by_fleet; body = %s", body.Code, w.Body.String())
	}
	// The refusal must tell the user where the delete path actually lives.
	if !strings.Contains(body.Error, "Cloud Runtime page") {
		t.Fatalf("error lacks actionable guidance: %q", body.Error)
	}
}

func assertRuntimeRowExists(t *testing.T, runtimeID string) {
	t.Helper()
	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&count); err != nil {
		t.Fatalf("count runtime rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("fleet-managed runtime row was physically deleted (count=%d)", count)
	}
}

// TestRuntimeUpsertPreservesFleetManagedColumns proves a normal daemon
// re-registration cannot clear the server-written fleet provenance. The
// upserts deliberately omit managed_by/fleet_node_id from DO UPDATE, so the
// values survive the conflict branch: an unauthenticated re-register must not
// turn a protected row into a deletable one.
func TestRuntimeUpsertPreservesFleetManagedColumns(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID := fleetManagedRuntimeFixture(t)
	var nodeID string
	if err := testPool.QueryRow(ctx, `
		UPDATE agent_runtime
		SET fleet_node_id = gen_random_uuid()
		WHERE id = $1
		RETURNING fleet_node_id::text
	`, runtimeID).Scan(&nodeID); err != nil {
		t.Fatalf("seed fleet_node_id: %v", err)
	}

	w := httptest.NewRecorder()
	testHandler.DaemonRegister(w, newRequest(http.MethodPost, "/api/daemon/register", map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    "daemon-managed",
		"device_name":  "node",
		"runtimes": []any{map[string]any{
			"name": "claude", "type": "claude", "status": "online", "runtime_mode": "local",
		}},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("re-register status = %d, body = %s", w.Code, w.Body.String())
	}

	var managedBy string
	var gotNode *string
	var lastSeenSet bool
	if err := testPool.QueryRow(ctx, `
		SELECT managed_by, fleet_node_id::text, last_seen_at IS NOT NULL
		FROM agent_runtime WHERE id = $1
	`, runtimeID).Scan(&managedBy, &gotNode, &lastSeenSet); err != nil {
		t.Fatalf("read runtime after re-register: %v", err)
	}
	if !lastSeenSet {
		t.Fatal("re-registration did not update the existing runtime row")
	}
	if managedBy != "local_fleet" {
		t.Fatalf("managed_by = %q, want local_fleet preserved", managedBy)
	}
	if gotNode == nil || *gotNode != nodeID {
		t.Fatalf("fleet_node_id = %v, want %q preserved", gotNode, nodeID)
	}
}

// TestDeleteManagedRuntimeIsRejected covers every ordinary runtime-delete
// entry point: the strict DELETE, the confirmed unbind-and-delete path (also
// served to installed clients on the legacy archive-agents-and-delete route),
// and the profile cascade. All must refuse with the structured 409 and leave
// the runtime row in place as the node tombstone.
func TestDeleteManagedRuntimeIsRejected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	t.Run("delete", func(t *testing.T) {
		runtimeID := fleetManagedRuntimeFixture(t)
		w := httptest.NewRecorder()
		req := withURLParam(newRequest(http.MethodDelete, "/api/runtimes/"+runtimeID, nil), "runtimeId", runtimeID)
		testHandler.DeleteAgentRuntime(w, req)

		assertFleetManagedDeleteRefusal(t, w)
		assertRuntimeRowExists(t, runtimeID)
	})

	t.Run("unbind-agents-and-delete", func(t *testing.T) {
		runtimeID := fleetManagedRuntimeFixture(t)
		w := httptest.NewRecorder()
		req := withURLParam(newRequest(http.MethodPost, "/api/runtimes/"+runtimeID+"/unbind-agents-and-delete", map[string]any{
			"expected_active_agent_ids": []string{},
		}), "runtimeId", runtimeID)
		testHandler.UnbindAgentsAndDeleteRuntime(w, req)

		assertFleetManagedDeleteRefusal(t, w)
		assertRuntimeRowExists(t, runtimeID)
	})

	t.Run("legacy-archive-route", func(t *testing.T) {
		runtimeID := fleetManagedRuntimeFixture(t)
		w := httptest.NewRecorder()
		req := withURLParam(newRequest(http.MethodPost, "/api/runtimes/"+runtimeID+"/archive-agents-and-delete", map[string]any{
			"expected_active_agent_ids": []string{},
		}), "runtimeId", runtimeID)
		testHandler.UnbindAgentsAndDeleteRuntime(w, req)

		assertFleetManagedDeleteRefusal(t, w)
		assertRuntimeRowExists(t, runtimeID)
	})

	t.Run("profile-cascade", func(t *testing.T) {
		ctx := context.Background()
		profileID := insertRuntimeProfileFixture(t, ctx, "Fleet Managed Profile", "claude", "claude")
		runtimeID := insertProfileRuntimeFixture(t, ctx, profileID, "Fleet Managed Profile Runtime", "claude")
		if _, err := testPool.Exec(ctx,
			`UPDATE agent_runtime SET managed_by = 'local_fleet' WHERE id = $1`, runtimeID); err != nil {
			t.Fatalf("mark profile runtime fleet-managed: %v", err)
		}

		w := httptest.NewRecorder()
		req := withURLParams(newRequest(http.MethodDelete,
			"/api/workspaces/"+testWorkspaceID+"/runtime-profiles/"+profileID, nil),
			"id", testWorkspaceID, "profileId", profileID)
		testHandler.DeleteRuntimeProfile(w, req)

		assertFleetManagedDeleteRefusal(t, w)
		assertRuntimeRowExists(t, runtimeID)
		var profileRows int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM runtime_profile WHERE id = $1`, profileID).Scan(&profileRows); err != nil {
			t.Fatalf("count profile rows: %v", err)
		}
		if profileRows != 1 {
			t.Fatalf("profile row was deleted despite the refusal (count=%d)", profileRows)
		}
	})
}
