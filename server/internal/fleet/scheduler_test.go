package fleet

import (
	"context"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"testing"
	"time"
)

// Virtual stage time is compared with the actual production context deadlines.
// No wall-clock sleep or private production claim reconstruction is needed.
type budgetProvider struct {
	*workerProvider
	clock   *time.Time
	applied bool
	deleted bool
}

func (p *budgetProvider) Diagnose(ctx context.Context, n model.Node, ref model.OperationRef) (model.Observation, error) {
	*p.clock = p.clock.Add(4 * time.Second)
	if d, ok := ctx.Deadline(); !ok || !p.clock.Before(d) {
		return model.Observation{}, context.DeadlineExceeded
	}
	return p.workerProvider.Diagnose(ctx, n, ref)
}
func (p *budgetProvider) Apply(ctx context.Context, n model.Node, a model.Action) (model.Observation, error) {
	*p.clock = p.clock.Add(2 * time.Second)
	if d, ok := ctx.Deadline(); !ok || !p.clock.Before(d) {
		return model.Observation{}, context.DeadlineExceeded
	}
	p.applied = true
	o := p.observation
	o.ObservedAt = *p.clock
	return o, nil
}
func (p *budgetProvider) Delete(ctx context.Context, n model.Node, ref model.OperationRef) error {
	// Preliminary inspection, a valid separately bounded offline proof, reinspection/removals.
	for _, stage := range []time.Duration{time.Second, 4 * time.Second, 2 * time.Second} {
		*p.clock = p.clock.Add(stage)
		if d, ok := ctx.Deadline(); !ok || !p.clock.Before(d) {
			return context.DeadlineExceeded
		}
	}
	p.deleted = true
	return nil
}

type budgetLifecycle struct {
	fakeLifecycle
	clock     *time.Time
	persisted *bool
}

func (c budgetLifecycle) Result(ctx context.Context, o model.Observation) error {
	*c.clock = c.clock.Add(500 * time.Millisecond)
	if d, ok := ctx.Deadline(); !ok || !c.clock.Before(d) {
		return context.DeadlineExceeded
	}
	*c.persisted = true
	return c.fakeLifecycle.Result(ctx, o)
}

type budgetRepo struct {
	*workerRepo
	clock    *time.Time
	finished bool
}

func (f *budgetRepo) FinishDelete(ctx context.Context, id pgtype.UUID, g int64) error {
	*f.clock = f.clock.Add(500 * time.Millisecond)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.finished = true
	return f.workerRepo.FinishDelete(ctx, id, g)
}
func TestRecoveryScheduledLifecycleOriginalBudget(t *testing.T) {
	f, p, r := workerFixture(model.Reboot, true)
	clock := time.Now()
	persisted := false
	provider := &budgetProvider{workerProvider: p, clock: &clock}
	r.provider = provider
	r.claimLifecycle = func(ctx context.Context, s store.RecoverySnapshot) (lifecycleHandle, error) {
		clock = clock.Add(500 * time.Millisecond) // SQL claim/check overhead after diagnosis.
		f.snap.Operation.ActionClaimedAt = clock
		f.snap.Operation.ActionStartEpoch = f.snap.Node.StartEpoch
		f.snap.Operation.Phase = "applying"
		return budgetLifecycle{fakeLifecycle: fakeLifecycle{repo: f, original: f.snap, deadline: clock.Add(5 * time.Second)}, clock: &clock, persisted: &persisted}, nil
	}
	_ = r.Tick(context.Background())
	waitInitialization(t, r)
	if !provider.applied || !persisted {
		t.Fatalf("scheduler truncated valid diagnosis+claim+Apply+result: applied=%v result=%v", provider.applied, persisted)
	}
}
func TestRecoveryScheduledDeleteOriginalBudget(t *testing.T) {
	f, p, r := workerFixture(model.Delete, true)
	clock := time.Now()
	repo := &budgetRepo{workerRepo: f, clock: &clock}
	provider := &budgetProvider{workerProvider: p, clock: &clock}
	r.repo = repo
	r.provider = provider
	_ = r.Tick(context.Background())
	waitInitialization(t, r)
	if !provider.deleted || !repo.finished {
		t.Fatalf("scheduler truncated valid >5s canonical Delete/completion: delete=%v finished=%v", provider.deleted, repo.finished)
	}
}

type heldPhysicalProvider struct {
	*workerProvider
	physical func(context.Context)
}

func (p *heldPhysicalProvider) Apply(ctx context.Context, n model.Node, a model.Action) (model.Observation, error) {
	p.physical(ctx)
	if ctx.Err() != nil {
		return model.Observation{}, ctx.Err()
	}
	return p.observation, nil
}
func (p *heldPhysicalProvider) Delete(ctx context.Context, n model.Node, ref model.OperationRef) error {
	p.physical(ctx)
	return ctx.Err()
}
func (p *heldPhysicalProvider) Inspect(ctx context.Context, n model.Node) (model.Observation, error) {
	return model.Observation{ContainerID: n.ContainerID, DaemonID: n.DaemonID, StartEpoch: n.StartEpoch, Status: "running", Ready: true, Agents: []string{"claude"}, RuntimeCount: 1, ReportStatsKnown: true, ObservedAt: time.Now()}, nil
}
func TestRecoveryScheduledPhysicalCancellationJoins(t *testing.T) {
	for _, action := range []model.Action{model.Reboot, model.Delete} {
		t.Run(string(action), func(t *testing.T) {
			f, p, r := workerFixture(action, true)
			installLifecycleFake(r, f, false)
			entered, canceled, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			r.provider = &heldPhysicalProvider{workerProvider: p, physical: func(ctx context.Context) { close(entered); <-ctx.Done(); close(canceled); <-release; close(exited) }}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				cancel()
				t.Fatal("physical slot did not start")
			}
			cancel()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("service cancellation did not reach physical work")
			}
			select {
			case <-done:
				t.Fatal("Run returned before physical work joined")
			default:
			}
			close(release)
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("Run did not join physical slot")
			}
			select {
			case <-exited:
			default:
				t.Fatal("physical job leaked")
			}
			if len(f.events) != 0 {
				t.Fatal("canceled work persisted completion/error")
			}
			if e := r.Tick(context.Background()); e != context.Canceled {
				t.Fatal("closed admission accepted work")
			}
		})
	}
}
func TestRecoveryScheduledPhysicalCurrentBindingFences(t *testing.T) {
	for _, action := range []model.Action{model.Reboot, model.Delete} {
		for _, stage := range []string{"before-effect", "before-completion"} {
			for _, change := range []string{"generation", "approval", "resources"} {
				t.Run(string(action)+"/"+stage+"/"+change, func(t *testing.T) {
					f, p, r := workerFixture(action, true)
					installLifecycleFake(r, f, false)
					mutate := func() {
						switch change {
						case "generation":
							f.snap.Node.Generation++
							f.snap.Operation.Generation++
						case "approval":
							f.snap.Operation.Approved = false
						case "resources":
							f.snap.Node.DataVolume = "changed"
						}
					}
					physical := 0
					r.provider = &heldPhysicalProvider{workerProvider: p, physical: func(context.Context) {
						physical++
						if stage == "before-completion" {
							mutate()
						}
					}}
					if stage == "before-effect" {
						f.current = func(calls int) error {
							if calls == 2 {
								mutate()
							}
							return nil
						}
					}
					_ = r.Tick(context.Background())
					waitInitialization(t, r)
					if stage == "before-effect" && physical != 0 {
						t.Fatal("stale accepted snapshot authorized physical effect")
					}
					if stage == "before-completion" && physical != 1 {
						t.Fatal("fixture did not reach result boundary")
					}
					for _, event := range f.events {
						if event == "result" || event == "finish-delete" {
							t.Fatal("changed authority persisted completion")
						}
					}
				})
			}
		}
	}
}
func TestRecoveryScheduledOneSharedSlotRetainsIdleHealth(t *testing.T) {
	f, p, r := workerFixture(model.Reboot, true)
	installLifecycleFake(r, f, false)
	first := f.snap
	repo := &backlogRepo{workerRepo: f, snapshots: map[pgtype.UUID]store.RecoverySnapshot{first.Operation.ID: first}, seen: map[pgtype.UUID]int{}, health: map[pgtype.UUID]time.Time{}}
	for _, action := range []model.Action{model.Create, model.Start} {
		s := first
		s.Node.ID.Bytes[0] = byte(4 + len(repo.snapshots))
		s.Operation.ID.Bytes[0] = byte(4 + len(repo.snapshots))
		s.Operation.NodeID = s.Node.ID
		s.Operation.Action = action
		s.Node.Maintenance = false
		s.Operation.Approved = false
		if action == model.Create {
			s.Node.ContainerID = ""
		}
		repo.snapshots[s.Operation.ID] = s
		repo.discovered = append(repo.discovered, s.Operation)
	}
	for _, id := range []byte{250, 251} {
		n := first.Node
		n.ID.Bytes[0] = id
		n.Maintenance = false
		repo.observable = append(repo.observable, n)
	}
	r.repo = repo
	claims := 0
	original := r.claimLifecycle
	r.claimLifecycle = func(ctx context.Context, s store.RecoverySnapshot) (lifecycleHandle, error) {
		claims++
		return original(ctx, s)
	}
	mints := 0
	r.claimBootstrap = func(context.Context, store.RecoverySnapshot) (bootstrapHandle, error) {
		mints++
		return nil, model.ErrConflict
	}
	entered, release := make(chan struct{}), make(chan struct{})
	r.provider = &heldPhysicalProvider{workerProvider: p, physical: func(context.Context) { close(entered); <-release }}
	_ = r.Tick(context.Background())
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("work not admitted")
	}
	for i := 0; i < 4; i++ {
		_ = r.Tick(context.Background())
	}
	if claims != 1 || mints != 0 {
		t.Fatal("occupied shared slot claimed or minted another operation")
	}
	if len(repo.health) != 2 {
		t.Fatal("physical slot starved supported-two idle health")
	}
	close(release)
	waitInitialization(t, r)
	if claims != 1 {
		t.Fatal("lifecycle redispatched")
	}
}
