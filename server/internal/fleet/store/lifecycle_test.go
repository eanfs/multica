package store

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"strings"
	"testing"
	"time"
)

func TestRecoveryLifecycleCheckpointAndEpoch(t *testing.T) {
	now := time.Now()
	claimed := now.Add(-6 * time.Second)
	s := RecoverySnapshot{SQLNow: now, Node: model.Node{Generation: 3, Desired: "running", ContainerID: "cid", DaemonID: "daemon", StartEpoch: claimed.Add(-time.Hour).Format(time.RFC3339Nano), Maintenance: true}, Operation: model.Operation{Action: model.Reboot, Generation: 3, Phase: "applying", Approved: true, ActionClaimedAt: claimed, ActionStartEpoch: claimed.Add(-time.Hour).Format(time.RFC3339Nano), CreatedAt: claimed.Add(-time.Second)}}
	o := model.Observation{ContainerID: "cid", DaemonID: "daemon", Status: "running", StartEpoch: claimed.Add(time.Second).Format(time.RFC3339Nano), Ready: true, Agents: []string{"claude"}, RuntimeCount: 1, ObservedAt: now}
	if e := lifecycleObservation(s, o); e != nil {
		t.Fatalf("new epoch rejected: %v", e)
	}
	if got := lifecycleState(s); got != "recovery" {
		t.Fatalf("expired checkpoint=%s", got)
	}
	s.SQLNow = claimed.Add(time.Second)
	if got := lifecycleState(s); got != "live" {
		t.Fatalf("live checkpoint=%s", got)
	}
	s.SQLNow = now
	for _, name := range []string{"old-epoch", "before-checkpoint", "missing-checkpoint", "future-epoch", "missing-container", "offline-ready", "no-claude", "no-runtime", "foreign-daemon", "foreign-container"} {
		t.Run(name, func(t *testing.T) {
			q, v := s, o
			switch name {
			case "old-epoch":
				v.StartEpoch = q.Operation.ActionStartEpoch
			case "before-checkpoint":
				v.ObservedAt = claimed.Add(-time.Second)
			case "missing-checkpoint":
				q.Operation.ActionClaimedAt = time.Time{}
			case "future-epoch":
				v.StartEpoch = now.Add(time.Second).Format(time.RFC3339Nano)
			case "missing-container":
				v.ContainerID = ""
			case "offline-ready":
				v.Offline = true
			case "no-claude":
				v.Agents = nil
			case "no-runtime":
				v.RuntimeCount = 0
			case "foreign-daemon":
				v.DaemonID = "foreign"
			case "foreign-container":
				v.ContainerID = "foreign"
			}
			if lifecycleObservation(q, v) == nil {
				t.Fatal("unsafe completion accepted")
			}
		})
	}
	s.Operation.ActionClaimedAt = time.Time{}
	s.Operation.Phase = "queued"
	if got := lifecycleState(s); got != "new" {
		t.Fatalf("first dispatch=%s", got)
	}
	s.Operation.ActionClaimedAt = now.Add(time.Second)
	if lifecycleState(s) != "denied" {
		t.Fatal("future clock accepted")
	}
}
func TestRecoveryLifecycleStopEvidence(t *testing.T) {
	now := time.Now()
	s := RecoverySnapshot{SQLNow: now, Node: model.Node{Generation: 1, ContainerID: "cid", DaemonID: "daemon", StartEpoch: "epoch", Desired: "stopped", Maintenance: true}, Operation: model.Operation{Action: model.Stop, Generation: 1, Approved: true, Phase: "applying", ActionClaimedAt: now.Add(-6 * time.Second)}}
	o := model.Observation{ContainerID: "cid", Status: "stopped", ObservedAt: now}
	if lifecycleObservation(s, o) != nil {
		t.Fatal("owned stopped evidence rejected")
	}
	o.Status = "running"
	o.Ready = true
	if lifecycleObservation(s, o) == nil {
		t.Fatal("running stop completed")
	}
}
func TestRecoveryLifecyclePrivateClaimFences(t *testing.T) {
	now := time.Now()
	s := RecoverySnapshot{SQLNow: now, Node: model.Node{ContainerID: "cid", Generation: 1, Desired: "running"}, Operation: model.Operation{Action: model.Start, Phase: "applying", Generation: 1, ActionClaimedAt: now.Add(-time.Second)}}
	c := LifecycleClaim{snapshot: s, claimedAt: s.Operation.ActionClaimedAt, deadline: now.Add(time.Second)}
	if !validLifecycleClaim(c, s, now) {
		t.Fatal("live winner denied")
	}
	for _, name := range []string{"projection", "expired-local", "expired-sql", "new-generation", "new-epoch", "approval-changed", "other-checkpoint", "future-sql"} {
		t.Run(name, func(t *testing.T) {
			a, b := c, s
			local := now
			switch name {
			case "projection":
				a = LifecycleClaim{}
			case "expired-local":
				local = c.deadline
			case "expired-sql":
				b.SQLNow = b.Operation.ActionClaimedAt.Add(5 * time.Second)
			case "new-generation":
				b.Node.Generation++
			case "new-epoch":
				b.Node.StartEpoch = "new"
			case "approval-changed":
				b.Operation.Approved = true
			case "other-checkpoint":
				b.Operation.ActionClaimedAt = b.Operation.ActionClaimedAt.Add(time.Millisecond)
			case "future-sql":
				b.SQLNow = b.Operation.ActionClaimedAt.Add(-time.Second)
			}
			if validLifecycleClaim(a, b, local) {
				t.Fatal("stale claimant accepted")
			}
		})
	}
	ctx, cancel := c.Context(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(c.deadline) {
		t.Fatal("deadline renewed")
	}
}

type lifecycleSQLCapture struct {
	*recoveryCapture
	sql  string
	args []any
}
type lifecycleNoRows struct{}

func (lifecycleNoRows) Scan(...any) error { return pgx.ErrNoRows }
func (c *lifecycleSQLCapture) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	c.sql = sql
	c.args = args
	return lifecycleNoRows{}
}
func TestRecoveryLifecycleCompiledCAS(t *testing.T) {
	c := &lifecycleSQLCapture{}
	q := db.New(c)
	_, _ = q.FleetClaimLifecycle(context.Background(), db.FleetClaimLifecycleParams{Namespace: "owned", Generation: 7, Action: "reboot", Approved: true, ContainerID: "cid", StartEpoch: "epoch"})
	for _, required := range []string{"action_claimed_at IS NULL", "phase IN ('queued','prepared')", "o.approved=", "n.generation=o.generation", "n.container_id=", "n.start_epoch=", "NOT n.revoked", "clock_timestamp()", "o.attempts<5", "action_start_epoch="} {
		if !strings.Contains(c.sql, required) {
			t.Fatalf("missing compiled CAS guard %q", required)
		}
	}
	if len(c.args) != 9 || c.args[0] != "epoch" || c.args[1] != "owned" || c.args[5] != int64(7) || c.args[6] != "reboot" || c.args[7] != true || c.args[8] != "cid" {
		t.Fatalf("unbound original CAS args=%v", c.args)
	}
}
func (c *lifecycleSQLCapture) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.sql = sql
	c.args = args
	return nil, pgx.ErrNoRows
}
func TestRecoveryIdleCompiledReadScope(t *testing.T) {
	c := &lifecycleSQLCapture{}
	_, _ = db.New(c).FleetObservable(context.Background(), db.FleetObservableParams{Namespace: "owned"})
	for _, required := range []string{"n.namespace=", "n.container_id<>''", "NOT n.revoked", "NOT n.maintenance", "n.desired='running'", "n.id>", "NOT EXISTS", "o.generation=n.generation", "'queued','preparing','prepared','applying'", "ORDER BY n.id LIMIT 100"} {
		if !strings.Contains(c.sql, required) {
			t.Fatalf("idle discovery guard missing %s", required)
		}
	}
	if len(c.args) != 2 || c.args[0] != "owned" {
		t.Fatal("namespace not bound")
	}
	if strings.Contains(c.sql, "UPDATE") || strings.Contains(c.sql, "INSERT") {
		t.Fatal("discovery became writer")
	}
}
