package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func fakeConfig(ns string) model.Config {
	return model.Config{Namespace: ns, Image: "fake-image@sha256:test", Specs: map[string]model.Spec{"local-small": {CPUs: 2, MemoryBytes: 4294967296, Pids: 256, MaxRuns: 3}}}
}
func ownerUUID(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	v, e := util.ParseUUID(id)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func cleanProduced(t *testing.T, f *testutil.Fixture, ns string) {
	t.Helper()
	for _, table := range []string{"fleet_nodes", "fleet_node_operations", "fleet_node_credentials", "fleet_credential_profiles"} {
		f.Cleanup(t, "DELETE FROM "+table+" WHERE namespace=$1 AND owner_id=$2", ns, f.UserID)
	}
}

// Removing the replay-first branch must fail even when configuration/profile/quota changes.
func TestCreateIntentReplays(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-replay"
	owner := ownerUUID(t, f.UserID)
	cleanProduced(t, f, ns)
	profileID := f.FleetProfile(t, ns)
	cfg := fakeConfig(ns)
	s := New(pool, ns, WithProvisioningConfig(cfg), WithMaxNodes(1))
	cfg.Specs["local-small"] = model.Spec{CPUs: 99}
	cfg.Image = "mutated"
	req := model.CreateRequest{Name: "local", Spec: "local-small", IdempotencyKey: "request-1"}
	first, op1, replayed, e := s.CreateIntent(context.Background(), owner, req)
	if e != nil || replayed {
		t.Fatalf("first err=%v replay=%v", e, replayed)
	}
	if first.Image != "fake-image@sha256:test" || first.Resources.MaxRuns != 3 || first.Resources.CPUs != 2 || first.ProfileRef != profileID || op1.NodeID != first.ID || op1.Phase != "queued" || op1.Action != model.Create || first.DaemonID == "" {
		t.Fatalf("bad snapshot node=%+v op=%+v", first, op1)
	}
	if e = s.UpsertProfiles(context.Background(), map[pgtype.UUID]string{owner: ""}, 2); e != nil {
		t.Fatal(e)
	}
	s = New(pool, ns, WithMaxNodes(1)) // missing provisioning config must not block replay
	second, op2, replayed, e := s.CreateIntent(context.Background(), owner, req)
	if e != nil || !replayed || first.ID != second.ID || op1.ID != op2.ID || second.Resources != first.Resources {
		t.Fatalf("replay err=%v replay=%v", e, replayed)
	}
	req.Name = "different"
	if _, _, _, e = s.CreateIntent(context.Background(), owner, req); !errors.Is(e, model.ErrConflict) {
		t.Fatalf("payload conflict=%v", e)
	}
	nodes, e := s.ListNodes(context.Background(), owner, 10, 0)
	if e != nil || len(nodes) != 1 {
		t.Fatalf("list=%d err=%v", len(nodes), e)
	}
	raw, e := json.Marshal(nodes)
	if e != nil || strings.Contains(string(raw), "fake-fixture-marker") || strings.Contains(string(raw), "mcn_") {
		t.Fatal("domain node projection leaked secrets")
	}
	for _, table := range []string{"fleet_nodes", "fleet_node_operations", "fleet_credential_profiles"} {
		var records string
		if e := pool.QueryRow(context.Background(), "SELECT COALESCE(jsonb_agg(to_jsonb(r))::text,'[]') FROM "+table+" r WHERE namespace=$1 AND owner_id=$2", ns, owner).Scan(&records); e != nil {
			t.Fatal(e)
		}
		if strings.Contains(records, "fake-fixture-marker") || strings.Contains(records, "mcn_") {
			t.Fatal("SQL row leaked marker material")
		}
	}
}

// Same-key concurrency must return one identity, not a second intent or a quota failure.
func TestCreateIntentConcurrentReplay(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-replay-race"
	cleanProduced(t, f, ns)
	f.FleetProfile(t, ns)
	owner := ownerUUID(t, f.UserID)
	s := New(pool, ns, WithProvisioningConfig(fakeConfig(ns)), WithMaxNodes(1))
	type result struct {
		node   model.Node
		op     model.Operation
		replay bool
		err    error
	}
	out := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, o, r, e := s.CreateIntent(context.Background(), owner, model.CreateRequest{Name: "local", Spec: "local-small", IdempotencyKey: "same"})
			out <- result{n, o, r, e}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	var nodeID, opID pgtype.UUID
	replays := 0
	for r := range out {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if !nodeID.Valid {
			nodeID = r.node.ID
			opID = r.op.ID
		}
		if nodeID != r.node.ID || opID != r.op.ID {
			t.Fatal("concurrent replay created another identity")
		}
		if r.replay {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("replays=%d", replays)
	}
}

// Omitting the owner transaction lock would allow both requests to occupy the last slot.
func TestCreateIntentConcurrentQuota(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-quota"
	cleanProduced(t, f, ns)
	f.FleetProfile(t, ns)
	owner := ownerUUID(t, f.UserID)
	s := New(pool, ns, WithProvisioningConfig(fakeConfig(ns)), WithMaxNodes(1))
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, key := range []string{"one", "two"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			<-start
			_, _, _, e := s.CreateIntent(context.Background(), owner, model.CreateRequest{Name: k, Spec: "local-small", IdempotencyKey: k})
			results <- e
		}(key)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for e := range results {
		if e == nil {
			successes++
		} else if errors.Is(e, model.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflict=%d", successes, conflicts)
	}
	var nodes, ops int
	if e := pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM fleet_nodes WHERE namespace=$1 AND owner_id=$2),(SELECT count(*) FROM fleet_node_operations WHERE namespace=$1 AND owner_id=$2)", ns, owner).Scan(&nodes, &ops); e != nil || nodes != 1 || ops != 1 {
		t.Fatalf("atomic counts=%d/%d err=%v", nodes, ops, e)
	}
	f.Exec(t, "UPDATE fleet_nodes SET desired='terminating', status='failed' WHERE namespace=$1 AND owner_id=$2", ns, owner)
	if _, _, _, e := s.CreateIntent(context.Background(), owner, model.CreateRequest{Name: "third", Spec: "local-small", IdempotencyKey: "third"}); !errors.Is(e, model.ErrConflict) {
		t.Fatalf("failed/terminating quota=%v", e)
	}
	f.Exec(t, "UPDATE fleet_nodes SET desired='terminated', status='terminated' WHERE namespace=$1 AND owner_id=$2", ns, owner)
	if _, _, _, e := s.CreateIntent(context.Background(), owner, model.CreateRequest{Name: "third", Spec: "local-small", IdempotencyKey: "third"}); e != nil {
		t.Fatalf("tombstone slot=%v", e)
	}
}

func TestCreateIntentRejectsMissingProfileAndInvalidSnapshot(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-missing-profile"
	owner := ownerUUID(t, f.UserID)
	cleanProduced(t, f, ns)
	req := model.CreateRequest{Name: "local", Spec: "local-small", IdempotencyKey: "one"}
	s := New(pool, ns, WithProvisioningConfig(fakeConfig(ns)))
	if _, _, _, e := s.CreateIntent(context.Background(), owner, req); !errors.Is(e, model.ErrProfileMissing) {
		t.Fatalf("missing profile=%v", e)
	}
	f.FleetProfile(t, ns)
	for _, cfg := range []model.Config{{}, fakeConfig("wrong"), {Namespace: ns, Image: "x", Specs: map[string]model.Spec{"local-small": {CPUs: 1}}}} {
		if _, _, _, e := New(pool, ns, WithProvisioningConfig(cfg)).CreateIntent(context.Background(), owner, req); !errors.Is(e, model.ErrInvalidRequest) {
			t.Fatalf("invalid config=%v", e)
		}
	}
	req.Spec = "unknown"
	if _, _, _, e := s.CreateIntent(context.Background(), owner, req); !errors.Is(e, model.ErrInvalidRequest) {
		t.Fatalf("unknown spec=%v", e)
	}
	id := ownerUUID(t, f.FleetNode(t, ns))
	f.Exec(t, "UPDATE fleet_nodes SET spec_config=$1 WHERE id=$2", []byte(`{"cpus":0,"memory_bytes":1,"pids":1,"max_runs":1}`), id)
	if _, e := s.GetNode(context.Background(), owner, id); !errors.Is(e, model.ErrUnavailable) {
		t.Fatalf("bad stored resources=%v", e)
	}
	if _, e := s.ListNodes(context.Background(), owner, 10, 0); !errors.Is(e, model.ErrUnavailable) {
		t.Fatalf("bad listed resources=%v", e)
	}
}

// Both writers must contend on the SAME owner protocol, including new-profile insertion.
func TestFleetOwnerLockCoordinatesIntentAndProfile(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-owner-lock"
	cleanProduced(t, f, ns)
	f.FleetProfile(t, ns)
	owner := ownerUUID(t, f.UserID)
	s := New(pool, ns, WithProvisioningConfig(fakeConfig(ns)))
	tx, e := pool.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if e = db.New(tx).FleetOwnerExclusiveLock(context.Background(), db.FleetOwnerExclusiveLockParams{Namespace: ns, OwnerID: owner}); e != nil {
		t.Fatal(e)
	}
	for _, fn := range []func(context.Context) error{
		func(ctx context.Context) error {
			_, _, _, e := s.CreateIntent(ctx, owner, model.CreateRequest{Name: "local", Spec: "local-small", IdempotencyKey: "one"})
			return e
		},
		func(ctx context.Context) error {
			return s.UpsertProfiles(ctx, map[pgtype.UUID]string{owner: "/new-private"}, 2)
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		e := fn(ctx)
		cancel()
		if e == nil {
			t.Fatal("writer bypassed held owner lock")
		}
	}
	if e = tx.Rollback(context.Background()); e != nil {
		t.Fatal(e)
	}
	p, e := s.GetProfile(context.Background(), owner)
	if e != nil || p.Version != 1 {
		t.Fatalf("blocked projection persisted=%v version=%d", e, p.Version)
	}
	nodes, e := s.ListNodes(context.Background(), owner, 10, 0)
	if e != nil || len(nodes) != 0 {
		t.Fatalf("blocked intent persisted=%v nodes=%d", e, len(nodes))
	}
}

func TestFleetProfileBatchRollback(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-profile-batch"
	cleanProduced(t, f, ns)
	ctx := context.Background()
	s := New(pool, ns)
	first := ownerUUID(t, f.UserID)
	second := ownerUUID(t, f.User(t, "batch owner", "batch-"+f.UserID+"@test.invalid"))
	f.Cleanup(t, "DELETE FROM fleet_credential_profiles WHERE namespace=$1 AND owner_id=$2", ns, second)
	low, high := first, second
	if util.UUIDToString(low) > util.UUIDToString(high) {
		low, high = high, low
	}
	if e := s.UpsertProfiles(ctx, map[pgtype.UUID]string{low: "/low-old"}, 2); e != nil {
		t.Fatal(e)
	}
	if e := s.UpsertProfiles(ctx, map[pgtype.UUID]string{high: "/high-old"}, 3); e != nil {
		t.Fatal(e)
	}
	// Low sorts first and is written; high then conflicts, requiring real transaction rollback.
	if e := s.UpsertProfiles(ctx, map[pgtype.UUID]string{low: "/low-new", high: "/high-new"}, 3); !errors.Is(e, model.ErrConflict) {
		t.Fatalf("batch conflict=%v", e)
	}
	p, e := s.GetProfile(ctx, low)
	if e != nil || p.Ref != "/low-old" || p.Version != 2 {
		t.Fatalf("batch partly persisted=%+v err=%v", p, e)
	}
	if e := s.UpsertProfiles(ctx, map[pgtype.UUID]string{high: "/stale"}, 2); !errors.Is(e, model.ErrConflict) {
		t.Fatalf("stale positive version=%v", e)
	}
}

// Request budget includes pool acquisition, not just the PostgreSQL transaction lifetime.
func TestCreateIntentBudgetIncludesPoolWait(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-intent-budget"
	cleanProduced(t, f, ns)
	f.FleetProfile(t, ns)
	owner := ownerUUID(t, f.UserID)
	s := New(pool, ns, WithProvisioningConfig(fakeConfig(ns)))
	held := make([]*pgxpool.Conn, 0, 4)
	for i := 0; i < 4; i++ {
		conn, e := pool.Acquire(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		held = append(held, conn)
	}
	defer held[0].Release()
	tx, e := held[0].Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if e = db.New(tx).FleetOwnerExclusiveLock(context.Background(), db.FleetOwnerExclusiveLockParams{Namespace: ns, OwnerID: owner}); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, _, _, e := s.CreateIntent(context.Background(), owner, model.CreateRequest{Name: "budget", Spec: "local-small", IdempotencyKey: "one"})
		done <- e
	}()
	<-time.NewTimer(600 * time.Millisecond).C
	for _, conn := range held[1:] {
		conn.Release()
	}
	e = <-done
	if e == nil {
		t.Fatal("intent bypassed held owner lock")
	}
	if elapsed := time.Since(started); elapsed > 2300*time.Millisecond {
		t.Fatalf("intent request budget omitted pool wait: elapsed=%s err=%v", elapsed, e)
	}
}

// Replacement or ignored versions break stable identity and atomic per-owner projection.
func TestFleetProfileProjectionVersionsAndBinding(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-profile"
	cleanProduced(t, f, ns)
	owner := ownerUUID(t, f.UserID)
	s := New(pool, ns, WithProvisioningConfig(fakeConfig(ns)))
	ref := "/test/private-one.json"
	ctx := context.Background()
	if e := s.UpsertProfiles(ctx, map[pgtype.UUID]string{owner: ref}, 1); e != nil {
		t.Fatal(e)
	}
	p, e := s.GetProfile(ctx, owner)
	if e != nil || p.Ref != ref || p.Version != 1 || p.Namespace != ns {
		t.Fatalf("profile=%+v err=%v", p, e)
	}
	for _, v := range []struct {
		ref     string
		version int64
	}{{ref, 0}, {"/different", 1}} {
		if e := s.UpsertProfiles(ctx, map[pgtype.UUID]string{owner: v.ref}, v.version); !errors.Is(e, model.ErrConflict) {
			t.Fatalf("version conflict=%v", e)
		}
	}
	if e = s.UpsertProfiles(ctx, map[pgtype.UUID]string{owner: ref}, 1); e != nil {
		t.Fatal(e)
	}
	node, _, _, e := s.CreateIntent(ctx, owner, model.CreateRequest{Name: "local", Spec: "local-small", IdempotencyKey: "one"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.UpsertProfiles(ctx, map[pgtype.UUID]string{}, 2); e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetProfileForNode(ctx, node); e != nil {
		t.Fatal("omission disabled profile")
	}
	if e = s.UpsertProfiles(ctx, map[pgtype.UUID]string{owner: "/test/private-two.json"}, 2); e != nil {
		t.Fatal(e)
	}
	p2, e := s.GetProfileForNode(ctx, node)
	if e != nil || p2.ID != p.ID || p2.Version != 2 {
		t.Fatalf("binding=%+v err=%v", p2, e)
	}
	forged := node
	forged.ProfileRef = "not-a-profile"
	if _, e = s.GetProfileForNode(ctx, forged); !errors.Is(e, model.ErrProfileMissing) {
		t.Fatalf("forged binding=%v", e)
	}
	forged = node
	forged.Namespace = "elsewhere"
	if _, e = s.GetProfileForNode(ctx, forged); !errors.Is(e, model.ErrProfileMissing) {
		t.Fatalf("namespace=%v", e)
	}
	otherID := f.User(t, "other profile owner", "profile-other-"+f.UserID+"@test.invalid")
	other := ownerUUID(t, otherID)
	f.Cleanup(t, "DELETE FROM fleet_credential_profiles WHERE namespace=$1 AND owner_id=$2", ns, other)
	if e = s.UpsertProfiles(ctx, map[pgtype.UUID]string{owner: "/bad-change", other: "/other"}, 2); !errors.Is(e, model.ErrConflict) {
		t.Fatalf("batch conflict=%v", e)
	}
	if _, e = s.GetProfile(ctx, other); !errors.Is(e, model.ErrProfileMissing) {
		t.Fatalf("partial batch=%v", e)
	}
	for _, bad := range []pgtype.UUID{{}, ownerUUID(t, "00000000-0000-0000-0000-000000000000"), ownerUUID(t, "11111111-1111-4111-8111-111111111111")} {
		if e = s.UpsertProfiles(ctx, map[pgtype.UUID]string{bad: "/private"}, 3); !errors.Is(e, model.ErrInvalidRequest) {
			t.Fatalf("invalid owner=%v", e)
		}
	}
	if e = s.UpsertProfiles(ctx, map[pgtype.UUID]string{owner: "relative"}, 3); !errors.Is(e, model.ErrInvalidRequest) {
		t.Fatalf("relative ref=%v", e)
	}
	if e = s.UpsertProfiles(ctx, map[pgtype.UUID]string{owner: ""}, 3); e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetProfileForNode(ctx, node); !errors.Is(e, model.ErrProfileMissing) {
		t.Fatalf("disabled=%v", e)
	}
	if _, _, _, e = s.CreateIntent(ctx, owner, model.CreateRequest{Name: "new", Spec: "local-small", IdempotencyKey: "new"}); !errors.Is(e, model.ErrProfileMissing) {
		t.Fatalf("disabled provision=%v", e)
	}
}
