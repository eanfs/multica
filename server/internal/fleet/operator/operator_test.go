package operator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
)

type fakeRepo struct {
	fence         store.NamespaceFence
	nodes         []model.Node
	op            model.Operation
	openCalls     int
	completionErr error
	finalErr      error
	beginCalls    int
	receipts      []store.NamespaceCompletion
	completion    func(store.NamespaceFence, pgtype.UUID, model.OperationRef, string) error
}

func (r *fakeRepo) CloseNamespace(_ context.Context, f, k string) (store.NamespaceFence, error) {
	if r.fence.Closed {
		if r.fence.FleetID != f || r.fence.OperationKey != k {
			return store.NamespaceFence{}, model.ErrConflict
		}
		return r.fence, nil
	}
	r.fence = store.NamespaceFence{Namespace: "fixture", FleetID: f, OperationKey: k, Generation: r.fence.Generation + 1, Closed: true}
	return r.fence, nil
}
func (r *fakeRepo) OpenNamespace(_ context.Context, f store.NamespaceFence) error {
	r.openCalls++
	r.fence.Closed = false
	return nil
}
func (r *fakeRepo) GetNamespaceFence(context.Context) (store.NamespaceFence, error) {
	return r.fence, nil
}
func (r *fakeRepo) ListNamespaceNodes(_ context.Context, after pgtype.UUID, limit int32) ([]model.Node, error) {
	out := []model.Node{}
	for _, n := range r.nodes {
		if !after.Valid || bytes.Compare(n.ID.Bytes[:], after.Bytes[:]) > 0 {
			out = append(out, n)
			if len(out) == int(limit) {
				break
			}
		}
	}
	return out, nil
}
func (r *fakeRepo) UpsertProfiles(context.Context, map[pgtype.UUID]string, int64) error { return nil }
func (r *fakeRepo) CheckNamespaceCompletion(_ context.Context, f store.NamespaceFence, expected model.Node, ref model.OperationRef, key string) error {
	owner := expected.OwnerID
	if r.completion != nil {
		return r.completion(f, owner, ref, key)
	}
	return r.completionErr
}
func (r *fakeRepo) CheckNamespaceCompletions(_ context.Context, f store.NamespaceFence, cs []store.NamespaceCompletion) error {
	if r.finalErr != nil {
		return r.finalErr
	}
	if f != r.fence {
		return model.ErrConflict
	}
	for _, c := range cs {
		for _, n := range r.nodes {
			if n.ID == c.Node.ID && n.Generation != c.Ref.Generation {
				return model.ErrConflict
			}
		}
	}
	r.receipts = append([]store.NamespaceCompletion(nil), cs...)
	r.fence.Finalized = true
	return nil
}
func (r *fakeRepo) BeginNamespaceDestroy(_ context.Context, f store.NamespaceFence) (store.NamespaceFence, error) {
	if f != r.fence || !f.Closed || !f.Finalized {
		return store.NamespaceFence{}, model.ErrConflict
	}
	r.beginCalls++
	r.fence.Generation++
	r.fence.Finalized = false
	return r.fence, nil
}
func (r *fakeRepo) LookupNamespaceOperation(_ context.Context, n model.Node, a model.Action, key string) (model.Operation, bool, error) {
	for _, c := range r.receipts {
		if c.Node.ID == n.ID && c.Key == key && c.Ref.Action == a {
			return model.Operation{ID: c.Ref.OperationID, OwnerID: n.OwnerID, NodeID: n.ID, Generation: c.Ref.Generation, Action: a, Approved: true, Phase: "completed"}, true, nil
		}
	}
	return model.Operation{}, false, nil
}
func testID(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{15: b}, Valid: true} }
func testConfig() Config {
	return Config{Namespace: "fixture", FleetID: "fleet", OperationKey: "shutdown", Timeout: time.Second}
}
func fixtureRepo() *fakeRepo {
	n := model.Node{Namespace: "fixture", ID: testID(1), OwnerID: testID(2), Generation: 2, Status: "stopped", Desired: "stopped"}
	return &fakeRepo{nodes: []model.Node{n}, op: model.Operation{ID: testID(3), OwnerID: n.OwnerID, NodeID: n.ID, Generation: 2, Action: model.Stop, Approved: true, Phase: "completed"}}
}

// Returning nil on a busy owner would authorize the caller to stop the API.
func privateMap(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"owners":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestClosedFencePrepareAndResumeCAS(t *testing.T) {
	r := fixtureRepo()
	r.fence = store.NamespaceFence{Namespace: "fixture", FleetID: "foreign", OperationKey: "shutdown", Generation: 9, Closed: true}
	cfg := testConfig()
	cfg.ProfilesFile = privateMap(t)
	if err := run(context.Background(), "prepare", cfg, dependencies{repo: r}); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("foreign fence accepted profile prepare: %v", err)
	}
	r.fence.FleetID = cfg.FleetID
	if err := run(context.Background(), "prepare", cfg, dependencies{repo: r}); err != nil {
		t.Fatal(err)
	}
	if r.openCalls != 0 || !r.fence.Closed {
		t.Fatal("prepare reopened fence")
	}
	bad := cfg
	bad.OperationKey = "wrong"
	if err := run(context.Background(), "resume", bad, dependencies{repo: r}); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("wrong workflow reopened fence: %v", err)
	}
	if err := run(context.Background(), "status", cfg, dependencies{repo: r}); err != nil {
		t.Fatal(err)
	}
	if r.openCalls != 0 || !r.fence.Closed {
		t.Fatal("status reopened fence")
	}
	if err := run(context.Background(), "resume", cfg, dependencies{repo: r}); err != nil || r.openCalls != 1 || r.fence.Closed {
		t.Fatalf("deliberate resume failed: %v", err)
	}
}

func TestBusyQuiesceKeepsAPIAndFence(t *testing.T) {
	r := fixtureRepo()
	d := dependencies{repo: r, request: func(context.Context, pgtype.UUID, pgtype.UUID, model.Action, string) (model.Operation, error) {
		return model.Operation{}, model.ErrBusy
	}}
	err := run(context.Background(), "quiesce", testConfig(), d)
	if !errors.Is(err, model.ErrBusy) || !r.fence.Closed || r.openCalls != 0 {
		t.Fatalf("busy shutdown returned permission to stop API: err=%v closed=%v opens=%d", err, r.fence.Closed, r.openCalls)
	}
}

func TestQuiesceWaitsSameRefAcrossAllOwnerPages(t *testing.T) {
	r := fixtureRepo()
	r.nodes = nil
	for i := 1; i <= 101; i++ {
		r.nodes = append(r.nodes, model.Node{Namespace: "fixture", ID: testID(byte(i)), OwnerID: testID(byte(200 + i%2)), Generation: 2})
	}
	requests := map[pgtype.UUID]string{}
	refs := map[pgtype.UUID]model.OperationRef{}
	polls := 0
	d := dependencies{repo: r, request: func(_ context.Context, owner, node pgtype.UUID, a model.Action, key string) (model.Operation, error) {
		if !r.fence.Closed {
			t.Fatal("node request preceded namespace close")
		}
		if a != model.Stop {
			t.Fatal("quiesce requested another action")
		}
		requests[node] = key
		return model.Operation{ID: testID(222), NodeID: node, OwnerID: owner, Action: a, Generation: 2, Approved: true, Phase: "queued"}, nil
	}}
	r.completion = func(f store.NamespaceFence, owner pgtype.UUID, ref model.OperationRef, key string) error {
		polls++
		if old, ok := refs[ref.NodeID]; ok && old != ref {
			t.Fatal("poll reconstructed operation ref")
		}
		refs[ref.NodeID] = ref
		if !f.Closed || owner != r.nodes[int(ref.NodeID.Bytes[15])-1].OwnerID || key != requests[ref.NodeID] {
			t.Fatal("completion lost SQL owner/original key")
		}
		if polls == 1 {
			return model.ErrBusy
		}
		return nil
	}
	cfg := testConfig()
	cfg.Timeout = 2 * time.Second
	if err := run(context.Background(), "quiesce", cfg, d); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 101 || len(refs) != 101 || polls != 102 {
		t.Fatalf("whole-owner completion incomplete: requests=%d refs=%d polls=%d", len(requests), len(refs), polls)
	}
	first := requests[testID(1)]
	if err := run(context.Background(), "quiesce", cfg, d); err != nil {
		t.Fatal(err)
	}
	if requests[testID(1)] != first {
		t.Fatal("workflow retry reminted node action key")
	}
}

func TestUnknownNodeIdentityPreservesClosedNamespace(t *testing.T) {
	r := fixtureRepo()
	r.nodes[0].ID = pgtype.UUID{Valid: true}
	r.op.NodeID = r.nodes[0].ID
	d := dependencies{repo: r, request: func(context.Context, pgtype.UUID, pgtype.UUID, model.Action, string) (model.Operation, error) {
		return r.op, nil
	}}
	if err := run(context.Background(), "quiesce", testConfig(), d); !errors.Is(err, model.ErrConflict) || !r.fence.Closed {
		t.Fatalf("zero SQL node identity authorized cleanup: %v", err)
	}
}

func TestQuiesceRequiresWholeNamespaceCompletionAtReturn(t *testing.T) {
	r := fixtureRepo()
	later := r.nodes[0]
	later.ID = testID(4)
	r.nodes = append(r.nodes, later)
	changed := false
	r.completion = func(_ store.NamespaceFence, _ pgtype.UUID, ref model.OperationRef, _ string) error {
		if ref.NodeID == later.ID && !changed {
			changed = true
			r.nodes[0].Generation++
			r.nodes[0].Status = "terminating"
			return model.ErrBusy
		}
		return nil
	}
	d := dependencies{repo: r, request: func(_ context.Context, owner, node pgtype.UUID, a model.Action, _ string) (model.Operation, error) {
		return model.Operation{ID: testID(node.Bytes[15] + 10), OwnerID: owner, NodeID: node, Generation: 2, Action: a, Approved: true, Phase: "completed"}, nil
	}}
	if err := run(context.Background(), "quiesce", testConfig(), d); !errors.Is(err, model.ErrConflict) || !r.fence.Closed || r.fence.Finalized || !changed {
		t.Fatalf("earlier stopped node changed during later polling: %v", err)
	}
}
func TestDestroyFromFinalizedStopUsesTrustedCycleCAS(t *testing.T) {
	r := fixtureRepo()
	requests := 0
	d := dependencies{repo: r, request: func(_ context.Context, owner, node pgtype.UUID, a model.Action, _ string) (model.Operation, error) {
		requests++
		if a == model.Delete {
			if r.fence.Finalized || r.beginCalls != 1 || r.fence.Generation != 2 {
				t.Fatal("destroy minted negative work without trusted closed cycle CAS")
			}
			r.nodes[0].Generation = 3
			return model.Operation{ID: testID(5), OwnerID: owner, NodeID: node, Generation: 3, Action: a, Approved: true, Phase: "completed"}, nil
		}
		return r.op, nil
	}}
	if e := run(context.Background(), "quiesce", testConfig(), d); e != nil || !r.fence.Finalized {
		t.Fatalf("stop finalization=%v", e)
	}
	if e := run(context.Background(), "destroy", testConfig(), d); e != nil || !r.fence.Finalized || !r.fence.Closed || r.beginCalls != 1 {
		t.Fatalf("destroy cycle=%v", e)
	}
	before := requests
	if e := run(context.Background(), "destroy", testConfig(), d); e != nil || requests != before || r.beginCalls != 1 {
		t.Fatalf("completed destroy reminted cycle/action: %v", e)
	}
}

// A completed SQL row alone is not proof that resources can be discarded.
func TestUnknownCleanupPreservesDatabase(t *testing.T) {
	r := fixtureRepo()
	r.completionErr = model.ErrUnknownHealth
	r.op.Action = model.Delete
	r.nodes[0].Status = "terminated"
	r.nodes[0].Desired = "terminated"
	d := dependencies{repo: r, request: func(context.Context, pgtype.UUID, pgtype.UUID, model.Action, string) (model.Operation, error) {
		return r.op, nil
	}}
	err := run(context.Background(), "destroy", testConfig(), d)
	if !errors.Is(err, model.ErrUnknownHealth) || !r.fence.Closed || r.openCalls != 0 {
		t.Fatalf("unknown resources authorized DB cleanup: err=%v closed=%v", err, r.fence.Closed)
	}
}
