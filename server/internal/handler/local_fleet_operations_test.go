package handler

import (
	"context"
	"encoding/json"
	"github.com/multica-ai/multica/server/internal/cloudruntime"
	"github.com/multica-ai/multica/server/internal/fleet"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalFleetPrivateReviewFullHop(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "task7-private-" + f.UserID
	t.Logf("Task7FixtureScope namespace=%s owner=%s workspace=%s", ns, f.UserID, f.WorkspaceID)
	node := f.FleetNode(t, ns, testutil.Cols{"status": "running", "container_id": "actual-container", "start_epoch": "current-epoch", "data_volume": "owned-data"})
	f.Cleanup(t, "DELETE FROM fleet_node_operations WHERE namespace=$1 AND owner_id=$2", ns, f.UserID)
	repo := store.New(pool, ns)
	owner := util.MustParseUUID(f.UserID)
	id := util.MustParseUUID(node)
	op, e := repo.PrepareMaintenance(context.Background(), owner, id, model.Stop, "review")
	if e != nil {
		t.Fatal(e)
	}
	key := []byte("test-private-012345678901234567890123456789")
	upstream := httptest.NewServer(fleet.NewService(repo, model.Config{Namespace: ns}, task7DiagnosticProvider{}).Handler(key))
	defer upstream.Close()
	h := &Handler{Queries: db.New(pool), TxStarter: pool, DB: pool, CloudRuntime: cloudruntime.NewClient(cloudruntime.Config{BaseURL: upstream.URL, ServiceSecret: key})}
	h.cfg.LocalFleetURL = upstream.URL
	api := httptest.NewServer(h.LocalFleetOperationsHandler(key))
	defer api.Close()
	c := cloudruntime.NewClient(cloudruntime.Config{BaseURL: api.URL, ServiceSecret: key})
	ref := model.OperationRef{Namespace: ns, NodeID: id, OperationID: op.ID, Generation: op.Generation, Action: op.Action}
	got, e := c.ReviewOperation(context.Background(), f.UserID, ref)
	if e != nil || !got.Approved || got.Phase != "queued" || got.OwnerID != owner || got.ID != op.ID || got.NodeID != id || got.Action != op.Action || got.Generation != op.Generation {
		t.Fatalf("actual review fullhop=%+v e=%v", got, e)
	}
	again, e := c.ReviewOperation(context.Background(), f.UserID, ref)
	if e != nil || again != got {
		t.Fatal("approved review not idempotent", e)
	}
	n, e := repo.GetNode(context.Background(), owner, id)
	if e != nil {
		t.Fatal(e)
	}
	dto, e := fleet.NewReviewResponse(n, got)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(dto)
	if e != nil {
		t.Fatal(e)
	}
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	if len(fields) != 8 {
		t.Fatalf("review contract=%s", raw)
	}
	for _, name := range []string{"namespace", "owner_id", "node_id", "operation_id", "generation", "action", "phase", "approved"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("missing field %s", name)
		}
	}
	request := fleet.OperationRequestDTO{Namespace: ns, NodeID: node, OperationID: util.UUIDToString(op.ID), Generation: op.Generation, Action: op.Action}
	body, _ := json.Marshal(request)
	call := func(secret, owner string, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/internal/local-fleet/operations/review", strings.NewReader(body))
		r.Header.Set("X-User-ID", owner)
		r.Header.Set("X-Fleet-Service-Key", secret)
		r.Header.Set("Authorization", "Bearer browser-token")
		w := httptest.NewRecorder()
		h.LocalFleetOperationsHandler(key).ServeHTTP(w, r)
		return w
	}
	if w := call("", f.UserID, string(body)); w.Code != 403 {
		t.Fatal("browser reached private handler", w.Code)
	}
	if w := call(string(key), node, string(body)); w.Code != 403 {
		t.Fatal("foreign owner reached review", w.Code)
	}
	if w := call(string(key), f.UserID, strings.Replace(string(body), ns, "foreign", 1)); w.Code != 403 {
		t.Fatal("foreign namespace reached review", w.Code)
	}
	if w := call(string(key), f.UserID, strings.TrimSuffix(string(body), "}")+",\"owner_id\":\""+f.UserID+"\"}"); w.Code != 400 {
		t.Fatal("caller owner field accepted", w.Code)
	}
	if w := call(string(key), f.UserID, strings.Replace(string(body), "\"generation\":2", "\"generation\":1", 1)); w.Code != 409 {
		t.Fatal("stale generation reviewed", w.Code)
	}
	n, e = repo.GetNode(context.Background(), owner, id)
	if e != nil || !n.Maintenance || n.Desired != "stopped" || n.Revoked {
		t.Fatal("review did physical/business cleanup", e)
	}
}
