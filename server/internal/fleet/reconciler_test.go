package fleet

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// workerRepo embeds the production type only to fail loudly on any unexpected seam.
// It models discovery versus fresh state separately; no claim fields are reconstructed.
type workerRepo struct {
	*store.Store
	snap         store.RecoverySnapshot
	discovered   []model.Operation
	currentCalls int
	current      func(int) error
	events       []string
	result       model.Observation
	profile      store.Profile
	observable   []model.Node
	observePages int
}

func (f *workerRepo) GetProfileForNode(_ context.Context, n model.Node) (store.Profile, error) {
	f.events = append(f.events, "profile")
	if f.profile.OwnerID != n.OwnerID {
		return store.Profile{}, model.ErrForbidden
	}
	return f.profile, nil
}
func (f *workerRepo) ListObservable(_ context.Context, after pgtype.UUID) ([]model.Node, error) {
	f.observePages++
	var nodes []model.Node
	for _, n := range f.observable {
		if after.Valid && strings.Compare(string(n.ID.Bytes[:]), string(after.Bytes[:])) <= 0 {
			continue
		}
		nodes = append(nodes, n)
		if len(nodes) == 100 {
			break
		}
	}
	return nodes, nil
}
func (f *workerRepo) ListRecoverable(context.Context) ([]model.Operation, error) {
	return f.discovered, nil
}
func (f *workerRepo) CurrentOperation(_ context.Context, owner pgtype.UUID, ref model.OperationRef) (store.RecoverySnapshot, error) {
	f.currentCalls++
	if ref != f.snap.Ref() || owner != f.snap.Node.OwnerID {
		return store.RecoverySnapshot{}, model.ErrConflict
	}
	if f.current != nil {
		if e := f.current(f.currentCalls); e != nil {
			return store.RecoverySnapshot{}, e
		}
	}
	return f.snap, nil
}
func (f *workerRepo) RecordOperationResult(_ context.Context, s store.RecoverySnapshot, o model.Observation) error {
	if s.Ref() != f.snap.Ref() {
		return model.ErrConflict
	}
	f.events = append(f.events, "result")
	f.result = o
	return nil
}
func (f *workerRepo) RecordObservation(_ context.Context, id pgtype.UUID, g int64, o model.Observation) error {
	if id != f.snap.Node.ID || g != f.snap.Node.Generation || (o.Ready && (f.snap.Node.Maintenance || f.snap.Node.Revoked)) {
		return model.ErrConflict
	}
	f.events = append(f.events, "observation")
	f.result = o
	return nil
}
func (f *workerRepo) DeferOperation(_ context.Context, s store.RecoverySnapshot, d time.Duration) error {
	if s.Ref() != f.snap.Ref() || d < 5*time.Second || d > 30*time.Second {
		return model.ErrConflict
	}
	f.events = append(f.events, "defer")
	f.snap.Operation.NextAttemptAt = f.snap.SQLNow.Add(d)
	return nil
}
func (f *workerRepo) RecordOperationError(_ context.Context, s store.RecoverySnapshot, code string, permanent bool) error {
	f.events = append(f.events, code)
	f.snap.Operation.Attempts++
	f.snap.Operation.NonRetryable = permanent || f.snap.Operation.Attempts >= 5
	return nil
}
func (f *workerRepo) ExpireBootstrap(_ context.Context, s store.RecoverySnapshot) error {
	if s.Ref() != f.snap.Ref() {
		return model.ErrConflict
	}
	f.events = append(f.events, "expire")
	f.snap.Operation.NonRetryable = true
	f.snap.Node.Revoked = true
	return nil
}
func (f *workerRepo) FinishDelete(_ context.Context, id pgtype.UUID, g int64) error {
	if id != f.snap.Operation.ID || g != f.snap.Node.Generation {
		return model.ErrConflict
	}
	f.events = append(f.events, "finish-delete")
	return nil
}
func workerFixture(action model.Action, approved bool) (*workerRepo, *workerProvider, *Reconciler) {
	id := func(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }
	now := time.Now()
	n := model.Node{ID: id(1), OwnerID: id(2), Namespace: "owned", Generation: 3, ContainerID: "cid", DaemonID: "daemon", StartEpoch: "epoch", DataVolume: "data", SecretsVolume: "secrets", Desired: "running", Status: "running", Maintenance: action == model.Delete || action == model.Stop || action == model.Reboot}
	if action == model.Delete {
		n.Desired = "terminating"
		n.Revoked = approved
		n.Status = "stopped"
	}
	if action == model.Stop {
		n.Desired = "stopped"
	}
	op := model.Operation{ID: id(3), NodeID: n.ID, OwnerID: n.OwnerID, Generation: 3, Action: action, Phase: "queued", Approved: approved, CreatedAt: now.Add(-time.Minute)}
	if n.Maintenance && !approved {
		op.Phase = "preparing"
	}
	f := &workerRepo{snap: store.RecoverySnapshot{Node: n, Operation: op, SQLNow: now}, discovered: []model.Operation{op}}
	p := &workerProvider{observation: model.Observation{ContainerID: n.ContainerID, DaemonID: n.DaemonID, StartEpoch: n.StartEpoch, Status: "running", Ready: true, Agents: []string{"claude"}, RuntimeCount: 1, ObservedAt: now, ReportStatsKnown: true}}
	r := &Reconciler{repo: f, provider: p, cfg: model.Config{Namespace: "owned"}}
	return f, p, r
}

type workerProvider struct {
	actions      []string
	observation  model.Observation
	err          error
	ref          model.OperationRef
	during       func()
	ensureCheck  func(context.Context, model.Node, model.Bootstrap)
	inspectCheck func(model.Node)
}

func (p *workerProvider) CheckAvailability(context.Context) error { return nil }
func (p *workerProvider) Ensure(ctx context.Context, n model.Node, b model.Bootstrap) (model.Observation, error) {
	p.actions = append(p.actions, "ensure")
	if p.ensureCheck != nil {
		p.ensureCheck(ctx, n, b)
	}
	if p.during != nil {
		p.during()
	}
	return p.observation, p.err
}
func (p *workerProvider) Inspect(_ context.Context, n model.Node) (model.Observation, error) {
	p.actions = append(p.actions, "inspect")
	if p.inspectCheck != nil {
		p.inspectCheck(n)
	}
	if p.during != nil {
		p.during()
	}
	return p.observation, p.err
}
func (p *workerProvider) Apply(context.Context, model.Node, model.Action) (model.Observation, error) {
	p.actions = append(p.actions, "apply")
	if p.during != nil {
		p.during()
	}
	return p.observation, p.err
}
func (p *workerProvider) Delete(_ context.Context, _ model.Node, ref model.OperationRef) error {
	p.actions = append(p.actions, "delete")
	if p.during != nil {
		p.during()
	}
	p.ref = ref
	return p.err
}
func (p *workerProvider) Diagnose(_ context.Context, _ model.Node, ref model.OperationRef) (model.Observation, error) {
	p.actions = append(p.actions, "diagnose")
	p.ref = ref
	return p.observation, p.err
}

type workerReviewer func(context.Context, string, model.OperationRef) (model.Operation, error)

func (f workerReviewer) ReviewOperation(c context.Context, o string, r model.OperationRef) (model.Operation, error) {
	return f(c, o, r)
}

func TestRecoveryReviewUsesSameOperation(t *testing.T) {
	f, p, r := workerFixture(model.Delete, false)
	original := f.snap.Ref()
	calls := 0
	r.SetReviewer(workerReviewer(func(_ context.Context, owner string, ref model.OperationRef) (model.Operation, error) {
		calls++
		if ref != original || owner == "" {
			t.Fatal("review changed original identity")
		}
		return model.Operation{Approved: true}, nil
	}))
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if calls != 1 || len(p.actions) != 0 || !reflect.DeepEqual(f.events, []string{"defer"}) || f.currentCalls < 2 {
		t.Fatalf("projection became approval: review=%d actions=%v events=%v current=%d", calls, p.actions, f.events, f.currentCalls)
	}
}
func TestRecoveryUnknownOfflinePreservesBarrier(t *testing.T) {
	f, p, r := workerFixture(model.Delete, true)
	op := f.snap.Operation
	p.observation = model.Observation{Status: "stopped", Offline: true, DataVolume: "data", LayoutVersion: "1", ObservedAt: f.snap.SQLNow}
	f.snap.Node.Observation = p.observation
	p.err = model.ErrUnknownHealth
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(p.actions, []string{"delete", "diagnose"}) || !reflect.DeepEqual(f.events, []string{"observation", "defer"}) || !f.snap.Node.Observation.Offline || f.snap.Node.Observation.ReportStatsKnown || f.snap.Operation.ID != op.ID || !f.snap.Operation.Approved || f.snap.Operation.Phase != op.Phase || f.snap.Operation.Attempts != 0 || !f.snap.Node.Maintenance || f.snap.Node.DataVolume != "data" || f.result.Ready {
		t.Fatalf("lost barrier: actions=%v events=%v snap=%+v", p.actions, f.events, f.snap)
	}
}
func TestRecoveryConfirmedCreateInspectOnly(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	for i := 0; i < 2; i++ {
		if e := r.Tick(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if !reflect.DeepEqual(p.actions, []string{"inspect", "inspect"}) || len(f.events) != 2 {
		t.Fatalf("actions=%v events=%v", p.actions, f.events)
	}
}
func TestRecoveryConfirmedMissingNeverReplaces(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	p.observation = model.Observation{Status: "missing", ObservedAt: f.snap.SQLNow}
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(p.actions, []string{"inspect"}) || !reflect.DeepEqual(f.events, []string{"observation", "instance-lost"}) || !f.snap.Operation.NonRetryable {
		t.Fatalf("actions=%v events=%v", p.actions, f.events)
	}
}
func TestRecoveryBootstrapCrashWindows(t *testing.T) {
	for _, name := range []string{"before-mint", "after-mint", "after-install", "before-confirmation"} {
		t.Run(name, func(t *testing.T) {
			f, p, r := workerFixture(model.Create, false)
			f.snap.Node.ContainerID = ""
			f.snap.Operation.Phase = "applying"
			f.snap.Operation.BootstrapClaimedAt = f.snap.SQLNow.Add(-6 * time.Minute)
			f.snap.Operation.BootstrapMinted = name != "before-mint"
			f.discovered = []model.Operation{f.snap.Operation}
			for i := 0; i < 2; i++ {
				if e := r.Tick(context.Background()); e != nil {
					t.Fatal(e)
				}
			}
			if len(p.actions) != 0 || !reflect.DeepEqual(f.events, []string{"expire"}) || f.snap.Node.DataVolume != "data" || !f.snap.Node.Revoked || f.snap.Operation.Phase != "applying" {
				t.Fatalf("actions=%v events=%v", p.actions, f.events)
			}
		})
	}
}
func TestRecoveryLiveBootstrapInert(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	f.snap.Node.ContainerID = ""
	f.snap.Operation.BootstrapClaimedAt = f.snap.SQLNow.Add(-time.Minute)
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(p.actions) != 0 || len(f.events) != 0 {
		t.Fatal("live claimant touched")
	}
}
func TestRecoveryDeleteCompletionRequiresSuccess(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failure], func(t *testing.T) {
			f, p, r := workerFixture(model.Delete, true)
			if failure {
				p.err = model.ErrUnavailable
			}
			_ = r.Tick(context.Background())
			finished := false
			for _, e := range f.events {
				finished = finished || e == "finish-delete"
			}
			if finished == failure {
				t.Fatalf("finish=%v events=%v actions=%v", finished, f.events, p.actions)
			}
			if !failure && p.ref != f.snap.Ref() {
				t.Fatal("delete replaced reference")
			}
		})
	}
}
func TestRecoveryReadyRequiresNativeHealth(t *testing.T) {
	for _, name := range []string{"wrong-epoch", "no-claude", "no-runtime", "offline", "stale", "future"} {
		t.Run(name, func(t *testing.T) {
			f, p, r := workerFixture(model.Create, false)
			switch name {
			case "wrong-epoch":
				p.observation.StartEpoch = "other"
			case "no-claude":
				p.observation.Agents = nil
			case "no-runtime":
				p.observation.RuntimeCount = 0
			case "offline":
				p.observation.Offline = true
			case "stale":
				p.observation.ObservedAt = f.snap.SQLNow.Add(-31 * time.Second)
			case "future":
				p.observation.ObservedAt = f.snap.SQLNow.Add(time.Second)
			}
			_ = r.Tick(context.Background())
			if len(p.actions) == 0 || f.result.Ready {
				t.Fatalf("invalid health became Ready actions=%v result=%+v", p.actions, f.result)
			}
		})
	}
}
func TestRecoveryStaleCurrentNeverActs(t *testing.T) {
	f, p, r := workerFixture(model.Delete, true)
	f.current = func(int) error { return model.ErrConflict }
	_ = r.Tick(context.Background())
	if len(p.actions) != 0 {
		t.Fatal("stale discovery acted")
	}
}
func TestRecoveryLateResultPreservesNewGeneration(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	p.during = func() { f.snap.Node.Generation++; f.snap.Operation.Generation++ }
	_ = r.Tick(context.Background())
	if len(f.events) != 0 {
		t.Fatal("stale physical result persisted")
	}
}
func TestRecoveryActualErrorBudget(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	p.err = errors.New("private provider path/key must not persist")
	for i := 0; i < 7; i++ {
		_ = r.Tick(context.Background())
	}
	if f.snap.Operation.Attempts != 5 || !f.snap.Operation.NonRetryable || len(p.actions) != 5 {
		t.Fatalf("attempts=%d actions=%v", f.snap.Operation.Attempts, p.actions)
	}
	for _, code := range f.events {
		if code != "unavailable" {
			t.Fatal("unsanitized error")
		}
	}
}

// An unapproved destructive action or an inactive phase must never become physically eligible.
type fakeLifecycle struct {
	repo      *workerRepo
	original  store.RecoverySnapshot
	deadline  time.Time
	failCheck bool
}

func (c fakeLifecycle) Context(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, c.deadline)
}
func (c fakeLifecycle) Snapshot() store.RecoverySnapshot { return c.original }
func (c fakeLifecycle) Check(ctx context.Context) (model.Node, error) {
	if c.failCheck || ctx.Err() != nil || !sameWorkerBinding(c.original, c.repo.snap) {
		return model.Node{}, model.ErrConflict
	}
	return c.repo.snap.Node, nil
}
func (c fakeLifecycle) Result(ctx context.Context, o model.Observation) error {
	if ctx.Err() != nil {
		return model.ErrConflict
	}
	return c.repo.RecordOperationResult(ctx, c.original, o)
}
func installLifecycleFake(r *Reconciler, f *workerRepo, failCheck bool) {
	var mu sync.Mutex
	r.claimLifecycle = func(_ context.Context, s store.RecoverySnapshot) (lifecycleHandle, error) {
		mu.Lock()
		defer mu.Unlock()
		if !f.snap.Operation.ActionClaimedAt.IsZero() || !sameWorkerBinding(s, f.snap) {
			return nil, model.ErrConflict
		}
		f.snap.Operation.ActionClaimedAt = f.snap.SQLNow
		f.snap.Operation.ActionStartEpoch = f.snap.Node.StartEpoch
		f.snap.Operation.Phase = "applying"
		return fakeLifecycle{repo: f, original: f.snap, deadline: time.Now().Add(5 * time.Second), failCheck: failCheck}, nil
	}
}
func TestRecoveryLifecycleCompetingReconcilers(t *testing.T) {
	f, p, r := workerFixture(model.Reboot, true)
	installLifecycleFake(r, f, false)
	other := &Reconciler{repo: f, provider: p, cfg: r.cfg, claimLifecycle: r.claimLifecycle}
	p.during = func() {
		if e := other.Tick(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := other.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(p.actions, []string{"diagnose", "apply"}) {
		t.Fatalf("duplicate dispatch %v", p.actions)
	}
	if f.snap.Operation.ActionClaimedAt.IsZero() || f.snap.Operation.Phase != "applying" {
		t.Fatal("checkpoint lost")
	}
}
func TestRecoveryLifecycleCheckpointNeverRedispatches(t *testing.T) {
	for _, name := range []string{"before-action", "after-action-before-result", "expired-original", "old-reboot-epoch"} {
		t.Run(name, func(t *testing.T) {
			f, p, r := workerFixture(model.Reboot, true)
			f.snap.Operation.ActionClaimedAt = f.snap.SQLNow.Add(-6 * time.Second)
			f.snap.Operation.ActionStartEpoch = f.snap.Node.StartEpoch
			f.snap.Operation.Phase = "applying"
			f.snap.Operation.IdempotencyKey = "original-key"
			before := f.snap.Operation
			for i := 0; i < 2; i++ {
				_ = r.Tick(context.Background())
			}
			if !reflect.DeepEqual(p.actions, []string{"inspect", "inspect"}) || f.snap.Operation.ID != before.ID || f.snap.Operation.Generation != before.Generation || f.snap.Operation.IdempotencyKey != before.IdempotencyKey || !f.snap.Operation.ActionClaimedAt.Equal(before.ActionClaimedAt) || !f.snap.Node.Maintenance {
				t.Fatalf("redispatch/reset: %v", p.actions)
			}
		})
	}
}
func TestRecoveryLifecycleClaimCheckBeforeApply(t *testing.T) {
	f, p, r := workerFixture(model.Start, false)
	installLifecycleFake(r, f, true)
	_ = r.Tick(context.Background())
	if len(p.actions) != 0 {
		t.Fatal("lost winner acted")
	}
	if f.snap.Operation.ActionClaimedAt.IsZero() {
		t.Fatal("missing durable checkpoint")
	}
}
func TestRecoveryConcurrentTickAndRunCancel(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	entered := make(chan struct{})
	release := make(chan struct{})
	p.during = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() { done <- r.Tick(context.Background()) }()
	<-entered
	if e := r.Tick(context.Background()); !errors.Is(e, model.ErrBusy) {
		t.Fatalf("concurrent Tick=%v", e)
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := r.Run(ctx); e != nil {
		t.Fatal(e)
	}
	if len(p.actions) != 1 || len(f.events) != 1 {
		t.Fatal("canceled Run dispatched")
	}
}

type fakeBootstrap struct {
	repo                    *workerRepo
	snapshot                store.RecoverySnapshot
	deadline                time.Time
	checks, mints, confirms int
	denyCheck               int
	expireOnMint            bool
}

func (c *fakeBootstrap) Context(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, c.deadline)
}
func (c *fakeBootstrap) Snapshot() store.RecoverySnapshot { return c.snapshot }
func (c *fakeBootstrap) Check(ctx context.Context) (model.Node, error) {
	c.checks++
	c.repo.events = append(c.repo.events, "check")
	if ctx.Err() != nil || c.checks == c.denyCheck || !sameWorkerBinding(c.snapshot, c.repo.snap) {
		return model.Node{}, model.ErrConflict
	}
	return c.repo.snap.Node, nil
}
func (c *fakeBootstrap) Mint(ctx context.Context) (string, int64, error) {
	c.mints++
	c.repo.events = append(c.repo.events, "mint")
	if c.expireOnMint {
		<-ctx.Done()
		return "", 0, ctx.Err()
	}
	return "owned-fake-token", 1, nil
}
func (c *fakeBootstrap) Confirm(ctx context.Context, o model.Observation) error {
	if ctx.Err() != nil || !sameWorkerBinding(c.snapshot, c.repo.snap) {
		return model.ErrConflict
	}
	c.confirms++
	c.repo.events = append(c.repo.events, "confirm")
	c.repo.snap.Node.ContainerID = o.ContainerID
	c.repo.snap.Node.StartEpoch = o.StartEpoch
	return nil
}
func (c *fakeBootstrap) Fail(context.Context, string) error {
	if !sameWorkerBinding(c.snapshot, c.repo.snap) {
		return model.ErrConflict
	}
	c.repo.events = append(c.repo.events, "fail-bootstrap")
	c.repo.snap.Node.Revoked = true
	c.repo.snap.Operation.NonRetryable = true
	return nil
}
func bootstrapFixture(t *testing.T) (*workerRepo, *workerProvider, *Reconciler, *fakeBootstrap) {
	t.Helper()
	f, p, r := workerFixture(model.Create, false)
	f.snap.Node.ContainerID = ""
	path := filepath.Join(t.TempDir(), "profile.json")
	if e := os.WriteFile(path, []byte(`{"api_key":"owned-fake-key"}`), 0600); e != nil {
		t.Fatal(e)
	}
	f.profile = store.Profile{OwnerID: f.snap.Node.OwnerID, Ref: path}
	r.cfg.APIURL = "http://owned.invalid"
	c := &fakeBootstrap{repo: f, deadline: time.Now().Add(5 * time.Minute)}
	r.claimBootstrap = func(_ context.Context, s store.RecoverySnapshot) (bootstrapHandle, error) {
		if !f.snap.Operation.BootstrapClaimedAt.IsZero() {
			return nil, model.ErrConflict
		}
		f.snap.Operation.BootstrapClaimedAt = f.snap.SQLNow
		f.snap.Operation.Phase = "applying"
		c.snapshot = f.snap
		return c, nil
	}
	return f, p, r, c
}
func TestRecoveryBootstrapWinningChoreography(t *testing.T) {
	f, p, r, c := bootstrapFixture(t)
	p.ensureCheck = func(ctx context.Context, n model.Node, b model.Bootstrap) {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(c.deadline) || b.NodeToken != "owned-fake-token" || b.APIKey != "owned-fake-key" || b.DaemonID != n.DaemonID || b.ServerURL != r.cfg.APIURL {
			t.Fatal("not actual original claim context/bootstrap")
		}
	}
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if c.mints != 1 || c.confirms != 1 || c.checks != 4 || !reflect.DeepEqual(f.events, []string{"check", "profile", "check", "check", "mint", "check", "confirm", "result"}) {
		t.Fatalf("sequence %v", f.events)
	}
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(p.actions, []string{"ensure", "inspect"}) || c.mints != 1 {
		t.Fatal("bootstrap duplicated")
	}
}
func TestRecoveryBootstrapEachBoundaryRechecked(t *testing.T) {
	for check := 1; check <= 4; check++ {
		t.Run(string(rune('0'+check)), func(t *testing.T) {
			f, p, r, c := bootstrapFixture(t)
			c.denyCheck = check
			if e := r.Tick(context.Background()); e != nil {
				t.Fatal(e)
			}
			if len(p.actions) != 0 || c.confirms != 0 || c.checks != check {
				t.Fatalf("lost claimant acted: %v", p.actions)
			}
			wantMint := 0
			if check == 4 {
				wantMint = 1
			}
			if c.mints != wantMint {
				t.Fatal("mint crossed stale authority")
			}
			if check == 1 {
				for _, event := range f.events {
					if event == "profile" {
						t.Fatal("profile read before claim check")
					}
				}
			}
		})
	}
}
func TestRecoveryBootstrapFailureIsFailClosed(t *testing.T) {
	f, p, r, c := bootstrapFixture(t)
	p.err = model.ErrUnavailable
	_ = r.Tick(context.Background())
	if c.mints != 1 || c.confirms != 0 || !f.snap.Node.Revoked || !f.snap.Operation.NonRetryable || f.snap.Node.DataVolume != "data" {
		t.Fatal("bootstrap failure lost guards")
	}
	_ = r.Tick(context.Background())
	if len(p.actions) != 1 {
		t.Fatal("failed bootstrap reminted/ensured")
	}
}
func TestRecoveryBootstrapOriginalDeadlineCannotRenew(t *testing.T) {
	_, p, r, c := bootstrapFixture(t)
	c.deadline = time.Now().Add(15 * time.Millisecond)
	c.expireOnMint = true
	_ = r.Tick(context.Background())
	if c.mints != 1 || len(p.actions) != 0 || c.confirms != 0 {
		t.Fatal("expired winner proceeded")
	}
}

func TestOperationRequiresApproval(t *testing.T) {
	for _, tc := range []struct {
		action         model.Action
		phase          string
		approved, want bool
	}{
		{model.Delete, "prepared", false, false}, {model.Delete, "prepared", true, true},
		{model.Stop, "queued", false, false}, {model.Stop, "applying", true, true},
		{model.Reboot, "queued", false, false}, {model.Reboot, "queued", true, true},
		{model.Create, "queued", false, true}, {model.Start, "prepared", false, true},
		{model.Delete, "preparing", true, false}, {model.Delete, "completed", true, false},
		{model.Start, "failed", true, false}, {model.Action("invalid"), "queued", true, false},
	} {
		op := model.Operation{Action: tc.action, Phase: tc.phase, Approved: tc.approved}
		if got := CanApplyOperation(op); got != tc.want {
			t.Errorf("action=%s phase=%s approved=%v: got %v want %v", tc.action, tc.phase, tc.approved, got, tc.want)
		}
	}
}
func TestRecoveryNativeRebootEpochDiscovery(t *testing.T) {
	f, p, r := workerFixture(model.Reboot, true)
	f.snap.Operation.Phase = "applying"
	f.snap.Operation.ActionClaimedAt = f.snap.SQLNow.Add(-6 * time.Second)
	f.snap.Operation.ActionStartEpoch = f.snap.Node.StartEpoch
	original := f.snap.Node
	p.inspectCheck = func(n model.Node) {
		if n.StartEpoch != "" || n.ContainerID != original.ContainerID || n.DaemonID != original.DaemonID || n.Resources != original.Resources || n.DataVolume != original.DataVolume {
			t.Fatal("native epoch discovery input lost binding")
		}
	}
	_ = r.Tick(context.Background())
	if f.snap.Node.StartEpoch != original.StartEpoch || f.snap.Operation.ActionStartEpoch != original.StartEpoch || !reflect.DeepEqual(p.actions, []string{"inspect"}) {
		t.Fatal("discovery adopted epoch/acted")
	}
}
func TestRecoveryDeleteLateSuccessPreservesNewGeneration(t *testing.T) {
	f, p, r := workerFixture(model.Delete, true)
	p.during = func() { f.snap.Node.Generation++; f.snap.Operation.Generation++ }
	_ = r.Tick(context.Background())
	if !reflect.DeepEqual(p.actions, []string{"delete"}) || len(f.events) != 0 {
		t.Fatal("late delete tombstoned new generation")
	}
}

func TestRecoveryIdleHealthBeyondCompletedOperation(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	f.discovered = nil
	f.snap.Operation.Phase = "completed"
	f.snap.Node.HealthAt = f.snap.SQLNow.Add(-31 * time.Second)
	f.observable = []model.Node{f.snap.Node}
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(p.actions, []string{"inspect"}) || !reflect.DeepEqual(f.events, []string{"observation"}) || !f.result.Ready || f.snap.Operation.Phase != "completed" {
		t.Fatalf("idle node never refreshed actions=%v events=%v", p.actions, f.events)
	}
}
func TestRecoveryIdleUnknownAgesOut(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	f.discovered = nil
	f.observable = []model.Node{f.snap.Node}
	p.observation.Ready = false
	before := f.snap.Node.HealthAt
	_ = r.Tick(context.Background())
	if len(p.actions) != 1 || len(f.events) != 0 || !f.snap.Node.HealthAt.Equal(before) {
		t.Fatal("unknown refreshed fabricated health")
	}
}
func TestRecoveryIdlePaginationAndCancellation(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	f.discovered = nil
	for i := 1; i <= 150; i++ {
		n := f.snap.Node
		n.ID.Bytes[0] = byte(i)
		f.observable = append(f.observable, n)
	}
	p.observation.Ready = false
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(p.actions) != 100 || r.observeAfter.Bytes[0] != 100 {
		t.Fatalf("first page=%d cursor=%d", len(p.actions), r.observeAfter.Bytes[0])
	}
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(p.actions) != 150 || r.observeAfter.Bytes[0] != 150 {
		t.Fatal("tail dropped")
	}
	if e := r.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if r.observeAfter.Valid {
		t.Fatal("empty tail did not reset cursor")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.during = cancel
	_ = r.Tick(ctx)
	if len(p.actions) != 151 || r.observeAfter.Bytes[0] != 1 {
		t.Fatal("cancellation dropped unprocessed page")
	}
}
func TestRecoveryIdleLateGenerationIsRejected(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	f.discovered = nil
	f.observable = []model.Node{f.snap.Node}
	p.during = func() { f.snap.Node.Generation++ }
	_ = r.Tick(context.Background())
	if len(f.events) != 0 {
		t.Fatal("late idle result adopted generation")
	}
}

func TestRecoveryIdleReadExclusions(t *testing.T) {
	for _, name := range []string{"namespace", "maintenance", "revoked", "unconfirmed", "desired-stopped"} {
		t.Run(name, func(t *testing.T) {
			f, p, r := workerFixture(model.Create, false)
			f.discovered = nil
			n := f.snap.Node
			switch name {
			case "namespace":
				n.Namespace = "foreign"
			case "maintenance":
				n.Maintenance = true
			case "revoked":
				n.Revoked = true
			case "unconfirmed":
				n.ContainerID = ""
			case "desired-stopped":
				n.Desired = "stopped"
			}
			f.observable = []model.Node{n}
			_ = r.Tick(context.Background())
			if len(p.actions) != 0 || len(f.events) != 0 {
				t.Fatal("ineligible idle identity inspected/written")
			}
		})
	}
}
func TestRecoveryIdleMaintenanceRaceIsRejected(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	f.discovered = nil
	f.observable = []model.Node{f.snap.Node}
	p.during = func() { f.snap.Node.Maintenance = true; f.snap.Node.Revoked = true }
	_ = r.Tick(context.Background())
	if len(f.events) != 0 || !f.snap.Node.Maintenance || !f.snap.Node.Revoked {
		t.Fatal("idle result crossed new delete barrier")
	}
}

func TestRecoveryBootstrapCompetingProcessAndLateGeneration(t *testing.T) {
	t.Run("live-loser", func(t *testing.T) {
		f, p, r, c := bootstrapFixture(t)
		other := &Reconciler{repo: f, provider: p, cfg: r.cfg, claimBootstrap: r.claimBootstrap}
		p.during = func() {
			if e := other.Tick(context.Background()); e != nil {
				t.Fatal(e)
			}
		}
		if e := r.Tick(context.Background()); e != nil {
			t.Fatal(e)
		}
		if c.mints != 1 || c.confirms != 1 || !reflect.DeepEqual(p.actions, []string{"ensure"}) {
			t.Fatal("competing bootstrap repeated")
		}
	})
	t.Run("late-generation", func(t *testing.T) {
		f, p, r, c := bootstrapFixture(t)
		p.during = func() { f.snap.Node.Generation++; f.snap.Operation.Generation++ }
		_ = r.Tick(context.Background())
		if c.mints != 1 || c.confirms != 0 || f.snap.Node.Revoked || f.snap.Node.ContainerID != "" || f.snap.Operation.NonRetryable {
			t.Fatal("late claimant confirmed/revoked new generation")
		}
	})
}
func TestRecoveryEpochNeutralizationScope(t *testing.T) {
	for _, name := range []string{"create", "stop", "no-checkpoint", "live-checkpoint", "unapproved"} {
		t.Run(name, func(t *testing.T) {
			f, p, r := workerFixture(model.Create, false)
			f.snap.Operation.Phase = "applying"
			f.snap.Operation.ActionClaimedAt = f.snap.SQLNow.Add(-6 * time.Second)
			f.snap.Operation.Approved = true
			switch name {
			case "stop":
				f.snap.Operation.Action = model.Stop
				f.snap.Node.Desired = "stopped"
				f.snap.Node.Maintenance = true
			case "no-checkpoint":
				f.snap.Operation.Action = model.Reboot
				f.snap.Node.Maintenance = true
				f.snap.Operation.ActionClaimedAt = time.Time{}
			case "live-checkpoint":
				f.snap.Operation.Action = model.Reboot
				f.snap.Node.Maintenance = true
				f.snap.Operation.ActionClaimedAt = f.snap.SQLNow.Add(-time.Second)
			case "unapproved":
				f.snap.Operation.Action = model.Reboot
				f.snap.Node.Maintenance = true
				f.snap.Operation.Approved = false
			}
			f.discovered = []model.Operation{f.snap.Operation}
			p.inspectCheck = func(n model.Node) {
				if n.StartEpoch != f.snap.Node.StartEpoch {
					t.Fatal("epoch cleared outside approved expired start/reboot")
				}
			}
			_ = r.Tick(context.Background())
		})
	}
}
func TestRecoveryStoppedMissingCompletesWithoutRedispatch(t *testing.T) {
	f, p, r := workerFixture(model.Stop, true)
	f.snap.Operation.Phase = "applying"
	f.snap.Operation.ActionClaimedAt = f.snap.SQLNow.Add(-6 * time.Second)
	p.observation = model.Observation{Status: "missing", ObservedAt: f.snap.SQLNow}
	_ = r.Tick(context.Background())
	if !reflect.DeepEqual(p.actions, []string{"inspect"}) || !reflect.DeepEqual(f.events, []string{"result"}) || f.snap.Operation.NonRetryable {
		t.Fatal("trusted stop absence became a failed barrier")
	}
}
func TestRecoveryIdleMissingDisablesReadyWithoutReplacement(t *testing.T) {
	f, p, r := workerFixture(model.Create, false)
	f.discovered = nil
	f.observable = []model.Node{f.snap.Node}
	p.observation = model.Observation{Status: "missing", ObservedAt: f.snap.SQLNow}
	_ = r.Tick(context.Background())
	if !reflect.DeepEqual(p.actions, []string{"inspect"}) || !reflect.DeepEqual(f.events, []string{"observation"}) || f.result.Ready || f.result.Status != "missing" || f.snap.Node.DataVolume != "data" {
		t.Fatal("missing identity remained Ready or was replaced")
	}
}
