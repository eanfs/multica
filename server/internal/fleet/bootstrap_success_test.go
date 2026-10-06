package fleet

import (
	"context"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"testing"
	"time"
)

type crashCompletionRepo struct {
	*workerRepo
	crash bool
}

func (f *crashCompletionRepo) RecordOperationResult(ctx context.Context, s store.RecoverySnapshot, o model.Observation) error {
	if f.crash {
		return model.ErrUnavailable
	}
	return f.workerRepo.RecordOperationResult(ctx, s, o)
}
func TestRecoveryHealthyBootstrapCompletionCrash(t *testing.T) {
	f, p, r, c := bootstrapFixture(t)
	repo := &crashCompletionRepo{workerRepo: f, crash: true}
	r.repo = repo
	p.ensureCheck = func(context.Context, model.Node, model.Bootstrap) {
		f.snap.SQLNow = f.snap.Operation.BootstrapClaimedAt.Add(5*time.Minute - time.Second)
		p.observation.ObservedAt = f.snap.SQLNow
	}
	_ = r.Tick(context.Background())
	waitInitialization(t, r)
	if c.confirms != 1 || !f.snap.Node.Ready {
		t.Fatal("fixture did not reach healthy confirmation crash boundary")
	}
	repo.crash = false
	f.snap.SQLNow = f.snap.Operation.BootstrapClaimedAt.Add(5*time.Minute + time.Second)
	p.observation.ObservedAt = f.snap.SQLNow
	_ = r.Tick(context.Background())
	waitInitialization(t, r)
	if f.snap.Node.Revoked || f.snap.Operation.NonRetryable || !f.result.Ready {
		t.Fatalf("healthy same-generation confirmed node revoked after completion crash: events=%v", f.events)
	}
	if c.mints != 1 || c.confirms != 1 {
		t.Fatal("recovery reminted or reconfirmed bootstrap")
	}
}
func TestRecoveryHealthyBootstrapCrashFreshnessAndRaces(t *testing.T) {
	for _, name := range []string{"stale-history-fresh-health", "unknown-health", "new-generation-during-inspect", "resources-during-inspect", "completed-during-inspect"} {
		t.Run(name, func(t *testing.T) {
			f, p, r, c := bootstrapFixture(t)
			repo := &crashCompletionRepo{workerRepo: f, crash: true}
			r.repo = repo
			p.ensureCheck = func(context.Context, model.Node, model.Bootstrap) {
				f.snap.SQLNow = f.snap.Operation.BootstrapClaimedAt.Add(5*time.Minute - time.Second)
				p.observation.ObservedAt = f.snap.SQLNow
			}
			_ = r.Tick(context.Background())
			waitInitialization(t, r)
			if c.confirms != 1 || !f.snap.BootstrapSucceeded {
				t.Fatal("did not reach persisted healthy crash boundary")
			}
			repo.crash = false
			f.snap.SQLNow = f.snap.SQLNow.Add(2 * time.Minute)
			p.observation.ObservedAt = f.snap.SQLNow
			switch name {
			case "unknown-health":
				p.err = model.ErrUnknownHealth
			case "new-generation-during-inspect":
				p.during = func() { f.snap.Node.Generation++; f.snap.Operation.Generation++ }
			case "resources-during-inspect":
				p.during = func() { f.snap.Node.DataVolume = "changed" }
			case "completed-during-inspect":
				p.during = func() { f.snap.Operation.Phase = "completed" }
			}
			_ = r.Tick(context.Background())
			waitInitialization(t, r)
			if f.snap.Node.Revoked || f.snap.Operation.NonRetryable {
				t.Fatal("historical success revoked on stale/unknown/raced health")
			}
			if f.result.Ready != (name == "stale-history-fresh-health") {
				t.Fatal("historical success manufactured current completion")
			}
			if c.mints != 1 || c.confirms != 1 {
				t.Fatal("history renewed/reminted bootstrap")
			}
		})
	}
}
func TestRecoveryHealthyBootstrapQueueStatsIndependent(t *testing.T) {
	for _, name := range []string{"unknown-reports", "pending-and-failed-reports"} {
		t.Run(name, func(t *testing.T) {
			f, p, r, c := bootstrapFixture(t)
			repo := &crashCompletionRepo{workerRepo: f, crash: true}
			r.repo = repo
			p.observation.ReportStatsKnown = name != "unknown-reports"
			p.observation.PendingReports = 2
			p.observation.FailedReports = 3
			p.ensureCheck = func(context.Context, model.Node, model.Bootstrap) {
				f.snap.SQLNow = f.snap.Operation.BootstrapClaimedAt.Add(5*time.Minute - time.Second)
				p.observation.ObservedAt = f.snap.SQLNow
			}
			_ = r.Tick(context.Background())
			waitInitialization(t, r)
			repo.crash = false
			f.snap.SQLNow = f.snap.SQLNow.Add(time.Minute)
			p.observation.ObservedAt = f.snap.SQLNow
			_ = r.Tick(context.Background())
			waitInitialization(t, r)
			if f.snap.Node.Revoked || f.snap.Operation.NonRetryable || !f.result.Ready || c.mints != 1 || c.confirms != 1 {
				t.Fatal("unknown/busy reports lost healthy initialization across completion crash")
			}
			if f.result.ReportStatsKnown != p.observation.ReportStatsKnown || f.result.PendingReports != 2 || f.result.FailedReports != 3 {
				t.Fatal("healthy completion rewrote report statistics")
			}
			nf, np, nr := workerFixture(model.Reboot, true)
			installLifecycleFake(nr, nf, false)
			np.observation.ReportStatsKnown = p.observation.ReportStatsKnown
			np.observation.PendingReports = 2
			np.observation.FailedReports = 3
			_ = joinedTick(t, nr, context.Background())
			for _, action := range np.actions {
				if action == "apply" {
					t.Fatal("initialization success weakened dangerous maintenance queue gate")
				}
			}
			if len(nf.events) != 1 || nf.events[0] != "defer" || !nf.snap.Node.Maintenance || nf.snap.Operation.Phase != "queued" || np.observation.PendingReports != 2 || np.observation.FailedReports != 3 {
				t.Fatal("unknown/busy maintenance proof altered guarded barriers or statistics")
			}
		})
	}
}
