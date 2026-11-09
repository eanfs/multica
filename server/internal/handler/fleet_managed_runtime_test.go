package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fleetManagedRuntimeFixture inserts a runtime already owned by a local fleet
// node. managed_by/fleet_node_id are server-written columns, so the row is
// inserted through the shared dbfx.Runtime builder with explicit overrides
// rather than routing it through a handler.
func fleetManagedRuntimeFixture(t *testing.T) string {
	t.Helper()
	if dbfx == nil {
		t.Skip("database not available")
	}
	return dbfx.Runtime(t, "claude", testutil.Cols{
		"daemon_id":    "daemon-managed",
		"runtime_mode": "local",
		"provider":     "claude",
		"device_info":  "node",
		"managed_by":   "local_fleet",
	})
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

// newCloudPATTestMiddleware returns DaemonAuth backed by a fake Fleet service.
// It passes nil queries on purpose: the verifier then skips the local-owner
// lookup, so the context it produces is usable without PostgreSQL.
func newCloudPATTestMiddleware(t *testing.T, ownerID, instanceRecordID string) func(http.Handler) http.Handler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"valid":true,"owner_id":%q,"instance_id":"i-01","instance_record_id":%q}`, ownerID, instanceRecordID)
	}))
	t.Cleanup(srv.Close)
	verifier := auth.NewCloudPATVerifier(auth.CloudPATVerifierConfig{FleetBaseURL: srv.URL})
	return middleware.DaemonAuth(nil, nil, nil, verifier)
}

// cloudPATRequestContext drives a request through DaemonAuth and returns the
// authenticated context, i.e. the context DaemonRegister would see.
func cloudPATRequestContext(t *testing.T, mw func(http.Handler) http.Handler) context.Context {
	t.Helper()
	var captured context.Context
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Context()
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/register", nil)
	req.Header.Set("Authorization", "Bearer mcn_test_token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cloud pat middleware status = %d, body = %s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("cloud pat middleware did not reach the downstream handler")
	}
	return captured
}

// TestCloudPATFleetMarkingRequiresLocalFleetMode pins the shared-verifier
// boundary: DaemonAuth attaches an mcn_ identity for SaaS Cloud and local
// Fleet alike, but only a local Fleet deployment may turn it into fleet
// provenance. Without this gate a SaaS runtime registered by a Cloud node
// would become undeletable and exempt from GC.
func TestCloudPATFleetMarkingRequiresLocalFleetMode(t *testing.T) {
	const (
		ownerID          = "01972f7e-7e8d-77ef-a13d-1b0ce3e9c001"
		instanceRecordID = "01972f7e-8a13-72a1-bbb0-0874ed4e8e67"
	)
	identityCtx := cloudPATRequestContext(t, newCloudPATTestMiddleware(t, ownerID, instanceRecordID))

	saas := &Handler{cfg: Config{LocalFleetEnabled: false}}
	if node, ok := saas.fleetNodeFromCloudPAT(identityCtx); ok {
		t.Fatalf("SaaS mode marked the runtime fleet-managed: node = %v", node)
	}

	local := &Handler{cfg: Config{LocalFleetEnabled: true}}
	node, ok := local.fleetNodeFromCloudPAT(identityCtx)
	if !ok {
		t.Fatal("local Fleet mode did not mark the runtime")
	}
	if got := uuidToString(node); got != instanceRecordID {
		t.Fatalf("fleet node = %q, want %q", got, instanceRecordID)
	}

	// A verified credential without a node record stays unmarked even in
	// local Fleet mode: a guessed node would attribute the runtime to the
	// wrong machine.
	noRecordCtx := cloudPATRequestContext(t, newCloudPATTestMiddleware(t, ownerID, ""))
	if node, ok := local.fleetNodeFromCloudPAT(noRecordCtx); ok {
		t.Fatalf("missing instance record marked the runtime: node = %v", node)
	}
}

// TestCloudPATRegistrationMarksFleetManagedOnlyInLocalFleetMode proves the
// full registration path, not just the helper: the same mcn_ credential
// registers an ordinary runtime in SaaS mode and a fleet-managed one in local
// Fleet mode.
func TestCloudPATRegistrationMarksFleetManagedOnlyInLocalFleetMode(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const instanceRecordID = "01972f7e-8a13-72a1-bbb0-0874ed4e8e67"
	mw := newCloudPATTestMiddleware(t, testUserID, instanceRecordID)
	ctx := context.Background()

	register := func(t *testing.T, daemonID string, localFleet bool) (string, *string) {
		t.Helper()
		h := *testHandler
		h.cfg.LocalFleetEnabled = localFleet
		w := httptest.NewRecorder()
		req := newRequest(http.MethodPost, "/api/daemon/register", map[string]any{
			"workspace_id": testWorkspaceID,
			"daemon_id":    daemonID,
			"device_name":  "cloud-pat-node",
			"runtimes": []any{map[string]any{
				"name": "claude", "type": "claude", "status": "online", "runtime_mode": "local",
			}},
		})
		req.Header.Set("Authorization", "Bearer mcn_test_token")
		mw(http.HandlerFunc(h.DaemonRegister)).ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("register status = %d, body = %s", w.Code, w.Body.String())
		}

		var runtimeID, managedBy string
		var fleetNodeID *string
		if err := testPool.QueryRow(ctx, `
			SELECT id::text, coalesce(managed_by, ''), fleet_node_id::text
			FROM agent_runtime
			WHERE workspace_id = $1 AND daemon_id = $2
		`, testWorkspaceID, daemonID).Scan(&runtimeID, &managedBy, &fleetNodeID); err != nil {
			t.Fatalf("read registered runtime: %v", err)
		}
		dbfx.Cleanup(t, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
		return managedBy, fleetNodeID
	}

	t.Run("saas-cloud", func(t *testing.T) {
		managedBy, fleetNodeID := register(t, "daemon-cloud-pat-saas", false)
		if managedBy != "" {
			t.Fatalf("managed_by = %q, want empty for a SaaS Cloud registration", managedBy)
		}
		if fleetNodeID != nil {
			t.Fatalf("fleet_node_id = %q, want NULL for a SaaS Cloud registration", *fleetNodeID)
		}
	})

	t.Run("local-fleet", func(t *testing.T) {
		managedBy, fleetNodeID := register(t, "daemon-cloud-pat-local", true)
		if managedBy != fleetManagedByLocalFleet {
			t.Fatalf("managed_by = %q, want %q", managedBy, fleetManagedByLocalFleet)
		}
		if fleetNodeID == nil || *fleetNodeID != instanceRecordID {
			t.Fatalf("fleet_node_id = %v, want %q", fleetNodeID, instanceRecordID)
		}
	})
}

// TestDeleteManagedRuntimeGCExcluded keeps the background runtime GC from
// physically deleting the tombstones that deletion protection relies on. The
// ordinary stale runtime is the control: without it an empty candidate batch
// would pass the test vacuously.
func TestDeleteManagedRuntimeGCExcluded(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	managedID := dbfx.Runtime(t, "fleet-managed gc candidate", testutil.Cols{
		"runtime_mode": "local",
		"provider":     "claude",
		"status":       "offline",
		"last_seen_at": testutil.Raw("now() - interval '30 days'"),
		"managed_by":   "local_fleet",
	})
	plainID := dbfx.Runtime(t, "ordinary stale runtime", testutil.Cols{
		"status":       "offline",
		"last_seen_at": testutil.Raw("now() - interval '30 days'"),
	})

	candidates, err := testHandler.Queries.ListStaleOfflineRuntimeGCCandidates(ctx, db.ListStaleOfflineRuntimeGCCandidatesParams{
		StaleSeconds: service.OfflineRuntimeTTLSeconds,
		MaxPerTick:   10000,
	})
	if err != nil {
		t.Fatalf("list GC candidates: %v", err)
	}
	seen := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		seen[uuidToString(id)] = true
	}
	if seen[managedID] {
		t.Fatal("fleet-managed runtime entered the GC candidate batch")
	}
	if !seen[plainID] {
		t.Fatal("ordinary stale runtime missing from the GC batch; test would pass vacuously")
	}

	eligible, err := testHandler.Queries.IsAgentRuntimeEligibleForGC(ctx, db.IsAgentRuntimeEligibleForGCParams{
		ID:           parseUUID(managedID),
		StaleSeconds: service.OfflineRuntimeTTLSeconds,
	})
	if err != nil {
		t.Fatalf("check managed runtime GC eligibility: %v", err)
	}
	if eligible {
		t.Fatal("fleet-managed runtime passed the locked GC re-check")
	}
	plainEligible, err := testHandler.Queries.IsAgentRuntimeEligibleForGC(ctx, db.IsAgentRuntimeEligibleForGCParams{
		ID:           parseUUID(plainID),
		StaleSeconds: service.OfflineRuntimeTTLSeconds,
	})
	if err != nil {
		t.Fatalf("check ordinary runtime GC eligibility: %v", err)
	}
	if !plainEligible {
		t.Fatal("ordinary stale runtime is not GC-eligible; fixture is wrong")
	}
}
