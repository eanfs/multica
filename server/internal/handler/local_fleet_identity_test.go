package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/fleet"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Allowing a valid node token to register an arbitrary daemon grants another machine's identity.
func TestLocalFleetIdentityRegistration(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task5-identity-" + f.UserID
	id := f.FleetNode(t, ns)
	nodeUUID, _ := util.ParseUUID(id)
	repo := store.New(pool, ns)
	cleanupTask5Fleet(t, pool, ns, f.UserID, f.WorkspaceID)
	token, _, err := repo.MintNodeToken(context.Background(), nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	node, err := db.New(pool).GetFleetNode(context.Background(), db.GetFleetNodeParams{Namespace: ns, OwnerID: parseUUID(f.UserID), NodeID: nodeUUID})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("test-only-012345678901234567890123456789")
	srv := httptest.NewServer(fleet.NewService(repo, model.Config{Namespace: ns}, nil).Handler(secret))
	defer srv.Close()
	verifier := auth.NewCloudPATVerifier(auth.CloudPATVerifierConfig{FleetBaseURL: srv.URL, ServiceSecret: secret})
	h := *testHandler
	h.Queries = db.New(pool)
	h.cfg.LocalFleetURL = srv.URL
	register := middleware.DaemonAuth(h.Queries, nil, nil, verifier)(http.HandlerFunc(h.DaemonRegister))
	call := func(daemon string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"workspace_id": f.WorkspaceID, "daemon_id": daemon, "legacy_daemon_ids": []string{"forged-legacy"}, "metadata": map[string]any{"managed_by": "forged", "fleet_node_id": "forged"}, "runtimes": []map[string]string{{"type": "claude", "name": "fake"}}})
		req := httptest.NewRequest("POST", "/api/daemon/register", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		register.ServeHTTP(w, req)
		return w
	}
	if w := call("forged-daemon"); w.Code != 403 {
		t.Fatalf("wrong daemon accepted: %d %s", w.Code, w.Body.String())
	}
	w := call(util.UUIDToString(node.DaemonID))
	if w.Code != 200 {
		t.Fatalf("valid binding rejected: %d %s", w.Code, w.Body.String())
	}
	var rows map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	runtimes, ok := rows["runtimes"].([]any)
	if !ok || len(runtimes) != 1 {
		t.Fatalf("registration runtimes=%v", rows)
	}
	registered := runtimes[0].(map[string]any)
	// Reconnect upsert keeps server metadata and the same runtime row.
	reconnect := call(util.UUIDToString(node.DaemonID))
	if reconnect.Code != 200 || !strings.Contains(reconnect.Body.String(), registered["id"].(string)) {
		t.Fatalf("reconnect=%d %s", reconnect.Code, reconnect.Body.String())
	}
	heartbeat := middleware.DaemonAuth(h.Queries, nil, nil, verifier)(http.HandlerFunc(h.DaemonHeartbeat))
	req := httptest.NewRequest("POST", "/api/daemon/heartbeat", strings.NewReader(`{"runtime_id":"`+registered["id"].(string)+`","metadata":{"managed_by":"forged"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	hb := httptest.NewRecorder()
	heartbeat.ServeHTTP(hb, req)
	if hb.Code != 200 {
		t.Fatalf("heartbeat=%d %s", hb.Code, hb.Body.String())
	}
	after := call(util.UUIDToString(node.DaemonID))
	if after.Code != 200 || !strings.Contains(after.Body.String(), "local_fleet") || !strings.Contains(after.Body.String(), id) {
		t.Fatalf("heartbeat lost protected metadata: %d %s", after.Code, after.Body.String())
	}
	raw := w.Body.String()
	if !strings.Contains(raw, "local_fleet") || !strings.Contains(raw, id) || strings.Contains(raw, "forged") {
		t.Fatalf("missing authoritative metadata: %s", raw)
	}
	if err := repo.RevokeNodeToken(context.Background(), nodeUUID); err != nil {
		t.Fatal(err)
	}
	if revoked := call(util.UUIDToString(node.DaemonID)); revoked.Code != 401 {
		t.Fatalf("same verifier accepted Store-revoked PAT: %d %s", revoked.Code, revoked.Body.String())
	}
}

func cleanupTask5Fleet(t *testing.T, pool *pgxpool.Pool, namespace, owner, workspace string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, sql := range []string{
			"DELETE FROM agent WHERE workspace_id=$1 AND runtime_id IN (SELECT id FROM agent_runtime WHERE workspace_id=$1 AND owner_id=$2)",
			"DELETE FROM agent_runtime WHERE workspace_id=$1 AND owner_id=$2",
		} {
			if _, err := pool.Exec(ctx, sql, workspace, owner); err != nil {
				t.Errorf("task5 runtime cleanup: %v", err)
			}
		}
		for _, table := range []string{"fleet_node_credentials", "fleet_node_operations", "fleet_nodes", "fleet_credential_profiles"} {
			if _, err := pool.Exec(ctx, "DELETE FROM "+table+" WHERE namespace=$1 AND owner_id=$2", namespace, owner); err != nil {
				t.Errorf("task5 %s cleanup: %v", table, err)
			}
			var count int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE namespace=$1 AND owner_id=$2", namespace, owner).Scan(&count); err != nil || count != 0 {
				t.Errorf("task5 %s remaining=%d err=%v", table, count, err)
			}
		}
		var remaining int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND owner_id=$2", workspace, owner).Scan(&remaining); err != nil || remaining != 0 {
			t.Errorf("task5 runtime remaining=%d err=%v", remaining, err)
		}
	})
}

func TestLocalFleetOrdinaryDaemonStripsManagedMetadata(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task5-ordinary-" + f.UserID
	cleanupTask5Fleet(t, pool, ns, f.UserID, f.WorkspaceID)
	h := *testHandler
	h.Queries = db.New(pool)
	h.cfg.LocalFleetURL = "http://127.0.0.1:19001"
	req := httptest.NewRequest("POST", "/api/daemon/register", strings.NewReader(`{"workspace_id":"`+f.WorkspaceID+`","daemon_id":"test-only-ordinary","metadata":{"managed_by":"local_fleet","fleet_node_id":"forged"},"runtimes":[{"type":"claude","name":"ordinary","metadata":{"managed_by":"local_fleet","fleet_node_id":"forged"}}]}`))
	req.Header.Set("X-User-ID", f.UserID)
	w := httptest.NewRecorder()
	h.DaemonRegister(w, req)
	if w.Code != 200 || strings.Contains(w.Body.String(), "managed_by") || strings.Contains(w.Body.String(), "fleet_node_id") {
		t.Fatalf("ordinary metadata trusted: %d %s", w.Code, w.Body.String())
	}
}

func TestLocalFleetIdentityRechecksSQLScopeAndCredential(t *testing.T) {
	for _, kind := range []string{"owner", "namespace", "revoked", "rotated"} {
		t.Run(kind, func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := "task5-scope-" + f.UserID
			cleanupTask5Fleet(t, pool, ns, f.UserID, f.WorkspaceID)
			id := f.FleetNode(t, ns)
			nodeID := parseUUID(id)
			repo := store.New(pool, ns)
			token, _, err := repo.MintNodeToken(context.Background(), nodeID)
			if err != nil {
				t.Fatal(err)
			}
			node, err := db.New(pool).GetFleetNode(context.Background(), db.GetFleetNodeParams{Namespace: ns, OwnerID: parseUUID(f.UserID), NodeID: nodeID})
			if err != nil {
				t.Fatal(err)
			}
			recordID := id
			switch kind {
			case "owner":
				other := f.User(t, "other", "task5-other-"+id+"@test.invalid")
				recordID = f.FleetNode(t, ns, testutil.Cols{"owner_id": other})
			case "namespace":
				recordID = f.FleetNode(t, ns+"-other")
			case "revoked":
				if _, err := pool.Exec(context.Background(), "UPDATE fleet_nodes SET revoked=true WHERE namespace=$1 AND id=$2", ns, nodeID); err != nil {
					t.Fatal(err)
				}
			case "rotated":
				if _, _, err := repo.MintNodeToken(context.Background(), nodeID); err != nil {
					t.Fatal(err)
				}
			}
			// A Fleet that mistakenly reports valid identity is not sufficient: SQL still owns scope/current credential.
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"valid": true, "owner_id": f.UserID, "instance_record_id": recordID, "instance_id": ""})
			}))
			defer upstream.Close()
			verifier := auth.NewCloudPATVerifier(auth.CloudPATVerifierConfig{FleetBaseURL: upstream.URL})
			h := *testHandler
			h.Queries = db.New(pool)
			h.cfg.LocalFleetURL = upstream.URL
			handler := middleware.DaemonAuth(h.Queries, nil, nil, verifier)(http.HandlerFunc(h.DaemonRegister))
			req := httptest.NewRequest("POST", "/api/daemon/register", strings.NewReader(`{"workspace_id":"`+f.WorkspaceID+`","daemon_id":"`+util.UUIDToString(node.DaemonID)+`","runtimes":[{"type":"claude","name":"fake"}]}`))
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 403 {
				t.Fatalf("%s SQL recheck bypassed: %d %s", kind, w.Code, w.Body.String())
			}
		})
	}
}

// Protected SQL identity survives an ordinary reconnect even after local mode is disabled.
func TestLocalFleetManagedUpsertCannotDowngrade(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task5-protected-" + f.UserID
	cleanupTask5Fleet(t, pool, ns, f.UserID, f.WorkspaceID)
	other := f.User(t, "other", "task5-upsert-"+f.UserID+"@test.invalid")
	f.Member(t, f.WorkspaceID, other, "member")
	id := f.Runtime(t, "managed", testutil.Cols{"daemon_id": "test-only-fixed-managed", "provider": "claude", "metadata": testutil.Raw(`'{"managed_by":"local_fleet","fleet_node_id":"22222222-2222-4222-8222-222222222222"}'::jsonb`)})
	h := *testHandler
	h.Queries = db.New(pool)
	h.cfg.LocalFleetURL = ""
	req := httptest.NewRequest("POST", "/api/daemon/register", strings.NewReader(`{"workspace_id":"`+f.WorkspaceID+`","daemon_id":"test-only-fixed-managed","runtimes":[{"type":"claude","name":"downgrade"}]}`))
	req.Header.Set("X-User-ID", other)
	w := httptest.NewRecorder()
	h.DaemonRegister(w, req)
	if w.Code != 403 {
		t.Fatalf("ordinary upsert changed managed binding: %d %s", w.Code, w.Body.String())
	}
	row, err := h.Queries.GetAgentRuntime(context.Background(), parseUUID(id))
	if err != nil || uuidToString(row.OwnerID) != f.UserID || !strings.Contains(string(row.Metadata), "local_fleet") {
		t.Fatalf("protected runtime changed: %v", err)
	}
}

func TestLocalFleetCustomAndFailedProfileMetadataProtected(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			pool, f := testutil.NewFleetFixture(t)
			ns := "task5-profile-" + f.UserID
			cleanupTask5Fleet(t, pool, ns, f.UserID, f.WorkspaceID)
			nodeID := parseUUID(f.FleetNode(t, ns))
			repo := store.New(pool, ns)
			token, _, err := repo.MintNodeToken(context.Background(), nodeID)
			if err != nil {
				t.Fatal(err)
			}
			node, err := db.New(pool).GetFleetNode(context.Background(), db.GetFleetNodeParams{Namespace: ns, OwnerID: parseUUID(f.UserID), NodeID: nodeID})
			if err != nil {
				t.Fatal(err)
			}
			profile := f.Insert(t, "runtime_profile", testutil.Cols{"workspace_id": f.WorkspaceID, "display_name": "fake profile", "protocol_family": "claude", "command_name": "/test-only-missing/claude", "created_by": f.UserID})
			secret := []byte("test-only-012345678901234567890123456789")
			upstream := httptest.NewServer(fleet.NewService(repo, model.Config{Namespace: ns}, nil).Handler(secret))
			defer upstream.Close()
			verifier := auth.NewCloudPATVerifier(auth.CloudPATVerifierConfig{FleetBaseURL: upstream.URL, ServiceSecret: secret})
			h := *testHandler
			h.Queries = db.New(pool)
			h.TxStarter = pool
			h.cfg.LocalFleetURL = upstream.URL
			body := map[string]any{"workspace_id": f.WorkspaceID, "daemon_id": util.UUIDToString(node.DaemonID)}
			if failed {
				body["failed_profiles"] = []map[string]string{{"profile_id": profile, "command_name": "/test-only-missing/claude", "reason": "fake missing fixture"}}
			} else {
				body["runtimes"] = []map[string]string{{"type": "claude", "name": "custom", "profile_id": profile}}
			}
			raw, _ := json.Marshal(body)
			register := middleware.DaemonAuth(h.Queries, nil, nil, verifier)(http.HandlerFunc(h.DaemonRegister))
			for range 2 {
				req := httptest.NewRequest("POST", "/api/daemon/register", strings.NewReader(string(raw)))
				req.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				register.ServeHTTP(w, req)
				if w.Code != 200 {
					t.Fatalf("trusted custom/failed upsert=%d %s", w.Code, w.Body.String())
				}
			}
			var runtimeID, owner string
			var metadata []byte
			f.QueryRow(t, "SELECT id::text,owner_id::text,metadata FROM agent_runtime WHERE workspace_id=$1 AND profile_id=$2", f.WorkspaceID, profile).Scan(&runtimeID, &owner, &metadata)
			if owner != f.UserID || !strings.Contains(string(metadata), "local_fleet") || !strings.Contains(string(metadata), util.UUIDToString(nodeID)) {
				t.Fatalf("custom/failed protected metadata=%s owner=%s", metadata, owner)
			}
			h.cfg.LocalFleetURL = ""
			req := httptest.NewRequest("POST", "/api/daemon/register", strings.NewReader(string(raw)))
			req.Header.Set("X-User-ID", f.UserID)
			w := httptest.NewRecorder()
			h.DaemonRegister(w, req)
			if w.Code != 403 {
				t.Fatalf("ordinary custom/failed downgrade=%d %s", w.Code, w.Body.String())
			}
			after, err := h.Queries.GetAgentRuntime(context.Background(), parseUUID(runtimeID))
			if err != nil || !strings.Contains(string(after.Metadata), "local_fleet") || uuidToString(after.OwnerID) != f.UserID {
				t.Fatalf("custom/failed binding changed: %v", err)
			}
		})
	}
}
