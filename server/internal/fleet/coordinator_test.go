package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/docker"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var errFakeStop = errors.New("fake stop failure")

// coordinatorTestService returns a Service bound to an isolated database. The
// caller-provided provider is the only Docker surface the coordinator sees.
func coordinatorTestService(t *testing.T, namespace string, provider *fakeDocker) (*Service, *store.Store) {
	t.Helper()
	pool := serviceTestPool(t)
	st := store.New(pool)
	return NewService(Config{Namespace: namespace}, st, provider, nil, "secret"), st
}

func seedCoordinatorNode(t *testing.T, st *store.Store, namespace, name, containerID string) db.FleetNode {
	t.Helper()
	return seedCoordinatorNodeWithTarget(t, st, namespace, name, containerID, StateRunning)
}

func seedCoordinatorNodeWithTarget(t *testing.T, st *store.Store, namespace, name, containerID, target string) db.FleetNode {
	t.Helper()
	return seedServiceNodeWith(t, st, db.CreateFleetNodeParams{
		OwnerID:       testOwner(60),
		Namespace:     namespace,
		Name:          name,
		ContainerID:   containerID,
		ContainerName: name + "-container",
		DaemonID:      name + "-daemon",
		Image:         "multica/runtime-claude:test",
		Spec:          []byte("{\"id\":\"standard\"}"),
		TargetState:   target,
	})
}

func seedCoordinatorOperation(t *testing.T, st *store.Store, nodeID pgtype.UUID, operation, phase, idempotencyKey string) db.FleetNodeOperation {
	t.Helper()
	op, err := st.Queries().CreateFleetNodeOperation(context.Background(), db.CreateFleetNodeOperationParams{
		NodeID:         nodeID,
		Operation:      operation,
		Phase:          phase,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		t.Fatalf("seed operation: %v", err)
	}
	return op
}

func loadCoordinatorOperation(t *testing.T, st *store.Store, id pgtype.UUID) db.FleetNodeOperation {
	t.Helper()
	op, err := st.Queries().GetFleetNodeOperation(context.Background(), id)
	if err != nil {
		t.Fatalf("load operation: %v", err)
	}
	return op
}

func TestStatusDetailFromHealthSeparatesRuntimeSignals(t *testing.T) {
	detail := statusDetailFromHealth(docker.HealthReport{Status: "running", Agents: []string{"claude"}})
	var got struct {
		CLIInstalled     bool `json:"cli_installed"`
		ModelConfigured  bool `json:"model_configured"`
		CredentialsValid bool `json:"credentials_valid"`
	}
	if err := json.Unmarshal(detail, &got); err != nil {
		t.Fatalf("status detail is not JSON: %v (%s)", err, detail)
	}
	if !got.CLIInstalled {
		t.Fatalf("cli_installed = false with a claude agent reported: %s", detail)
	}
	if !got.ModelConfigured {
		t.Fatalf("model_configured = false for a running daemon: %s", detail)
	}
	// Fleet never probes model credentials by default, so validity must not be
	// claimed just because the daemon is up.
	if got.CredentialsValid {
		t.Fatalf("credentials_valid = true without a credential check: %s", detail)
	}
}

func TestStatusDetailFromHealthReportsMissingAgent(t *testing.T) {
	detail := statusDetailFromHealth(docker.HealthReport{Status: "running", Agents: []string{"other"}})
	var got struct {
		CLIInstalled bool `json:"cli_installed"`
	}
	if err := json.Unmarshal(detail, &got); err != nil {
		t.Fatalf("status detail is not JSON: %v", err)
	}
	if got.CLIInstalled {
		t.Fatalf("cli_installed = true without a claude agent: %s", detail)
	}
}

func TestTickExecutesApprovedStopAndUpdatesObserved(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c1"] = docker.ObservedContainer{ID: "c1", State: "running", Healthy: true}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNode(t, st, "wt-op", "n", "c1")
	op := seedCoordinatorOperation(t, st, node.ID, "stop", OpPhaseApproved, "stop-1")

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(provider.stopped) != 1 || provider.stopped[0] != "c1" {
		t.Fatalf("stopped = %v", provider.stopped)
	}
	if updated := loadCoordinatorOperation(t, st, op.ID); updated.Phase != OpPhaseDone {
		t.Fatalf("phase = %q, want %q", updated.Phase, OpPhaseDone)
	}
	refreshed, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: node.ID, Namespace: "wt-op"})
	if err != nil {
		t.Fatalf("load refreshed node: %v", err)
	}
	if refreshed.ObservedState != StateRunning || !refreshed.DaemonReady {
		t.Fatalf("observed state = %q ready = %v", refreshed.ObservedState, refreshed.DaemonReady)
	}
	var detail struct {
		CLIInstalled bool `json:"cli_installed"`
	}
	if err := json.Unmarshal(refreshed.StatusDetail, &detail); err != nil {
		t.Fatalf("status detail: %v", err)
	}
	if !detail.CLIInstalled {
		t.Fatalf("status detail lost the observed agent: %s", refreshed.StatusDetail)
	}
}

func TestTickOnlyExecutesApprovedOperations(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-prepared"] = docker.ObservedContainer{ID: "c-prepared", State: "running", Healthy: true}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNode(t, st, "wt-op", "n-prepared", "c-prepared")
	op := seedCoordinatorOperation(t, st, node.ID, "stop", OpPhasePrepared, "stop-prepared")

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(provider.stopped) != 0 {
		t.Fatalf("unapproved operation reached Docker: %v", provider.stopped)
	}
	if got := loadCoordinatorOperation(t, st, op.ID).Phase; got != OpPhasePrepared {
		t.Fatalf("phase = %q, want unchanged %q", got, OpPhasePrepared)
	}
}

func TestTickIgnoresOperationsFromAnotherNamespace(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-own"] = docker.ObservedContainer{ID: "c-own", State: "running", Healthy: true}
	provider.inspected["c-foreign"] = docker.ObservedContainer{ID: "c-foreign", State: "running", Healthy: true}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	own := seedCoordinatorNode(t, st, "wt-op", "own", "c-own")
	foreign := seedCoordinatorNode(t, st, "other-ns", "foreign", "c-foreign")
	ownOp := seedCoordinatorOperation(t, st, own.ID, "stop", OpPhaseApproved, "own-stop")
	foreignOp := seedCoordinatorOperation(t, st, foreign.ID, "stop", OpPhaseApproved, "foreign-stop")

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(provider.stopped) != 1 || provider.stopped[0] != "c-own" {
		t.Fatalf("stopped = %v, want only the namespace node", provider.stopped)
	}
	if got := loadCoordinatorOperation(t, st, ownOp.ID).Phase; got != OpPhaseDone {
		t.Fatalf("own phase = %q, want %q", got, OpPhaseDone)
	}
	if got := loadCoordinatorOperation(t, st, foreignOp.ID).Phase; got != OpPhaseApproved {
		t.Fatalf("foreign phase = %q, want untouched %q", got, OpPhaseApproved)
	}
}

func TestTickExecutesApprovedDelete(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-delete"] = docker.ObservedContainer{ID: "c-delete", State: "running", Healthy: true}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNode(t, st, "wt-op", "n-delete", "c-delete")
	if _, err := st.Queries().CreateFleetNodeCredential(context.Background(), db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: "hash-delete",
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	op := seedCoordinatorOperation(t, st, node.ID, "delete", OpPhaseApproved, "delete-1")

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(provider.removed) != 1 || provider.removed[0] != "c-delete" {
		t.Fatalf("removed containers = %v", provider.removed)
	}
	if len(provider.removedVolumes) != 2 {
		t.Fatalf("removed volumes = %v", provider.removedVolumes)
	}
	deleted, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: node.ID, Namespace: "wt-op"})
	if err != nil {
		t.Fatalf("load deleted node: %v", err)
	}
	if deleted.TargetState != StateTerminated {
		t.Fatalf("target = %q, want %q", deleted.TargetState, StateTerminated)
	}
	if _, err := st.Queries().GetFleetNodeCredentialByHash(context.Background(), "hash-delete"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("credential still active after delete: %v", err)
	}
	if got := loadCoordinatorOperation(t, st, op.ID).Phase; got != OpPhaseDone {
		t.Fatalf("phase = %q, want %q", got, OpPhaseDone)
	}
}

func TestTickRequeuesFailedOperation(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-fail"] = docker.ObservedContainer{ID: "c-fail", State: "running", Healthy: true}
	provider.stopErr = errFakeStop
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNode(t, st, "wt-op", "n-fail", "c-fail")
	op := seedCoordinatorOperation(t, st, node.ID, "stop", OpPhaseApproved, "stop-fail")

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick must not abort on one failed operation: %v", err)
	}
	retried := loadCoordinatorOperation(t, st, op.ID)
	if retried.Phase != OpPhaseApproved {
		t.Fatalf("phase = %q, want requeued %q", retried.Phase, OpPhaseApproved)
	}
	if retried.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", retried.RetryCount)
	}
	if !strings.Contains(retried.Error, errFakeStop.Error()) {
		t.Fatalf("error = %q, want the Docker error", retried.Error)
	}
	if len(provider.stopped) != 0 {
		t.Fatalf("failed stop recorded as applied: %v", provider.stopped)
	}
}

func TestTickSkipsTerminalNodesDuringRefresh(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-gone"] = docker.ObservedContainer{}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNode(t, st, "wt-op", "n-terminated", "c-gone")
	if err := st.Queries().UpdateFleetNodeTargetState(context.Background(), db.UpdateFleetNodeTargetStateParams{
		ID: node.ID, Namespace: "wt-op", TargetState: StateTerminated,
	}); err != nil {
		t.Fatalf("mark terminated: %v", err)
	}
	// A terminated tombstone keeps its container id; the coordinator must not
	// reclassify it as a missing live instance.
	provider.inspected = map[string]docker.ObservedContainer{}

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("terminal node reached Docker: %v", provider.calls)
	}
}

func TestNextTargetStateAdvancesSettledTargets(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		settled string
		want    string
		changed bool
	}{
		{"starting ready", StateStarting, StateRunning, StateRunning, true},
		{"rebooting ready", StateRebooting, StateRunning, StateRunning, true},
		{"stopping exited", StateStopping, StateStopped, StateStopped, true},
		{"starting not ready", StateStarting, StateStarting, "", false},
		{"stopping still running", StateStopping, StateRunning, "", false},
		{"running exited", StateRunning, StateStopped, "", false},
		{"stopped", StateStopped, StateStopped, "", false},
		{"terminated", StateTerminated, StateRunning, "", false},
	}
	for _, tc := range cases {
		got, changed := nextTargetState(tc.target, nodeObservation{exists: true, settled: tc.settled})
		if changed != tc.changed || got != tc.want {
			t.Fatalf("%s: nextTargetState = %q/%v, want %q/%v", tc.name, got, changed, tc.want, tc.changed)
		}
	}
	if _, changed := nextTargetState(StateStarting, nodeObservation{}); changed {
		t.Fatal("a missing container must not advance the target state")
	}
}

func TestTickAdvancesStartingNodeToRunning(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-starting"] = docker.ObservedContainer{ID: "c-starting", State: "running", Healthy: true}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNodeWithTarget(t, st, "wt-op", "n-starting", "c-starting", StateStarting)

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	advanced, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: node.ID, Namespace: "wt-op"})
	if err != nil {
		t.Fatalf("load advanced node: %v", err)
	}
	if advanced.TargetState != StateRunning {
		t.Fatalf("target = %q, want %q", advanced.TargetState, StateRunning)
	}
	generation := advanced.Generation
	// The second pass observes the same settled state and must not bump the
	// generation again.
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	settled, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: node.ID, Namespace: "wt-op"})
	if err != nil {
		t.Fatalf("reload settled node: %v", err)
	}
	if settled.Generation != generation {
		t.Fatalf("generation moved from %d to %d without a state change", generation, settled.Generation)
	}
}

func TestTickAdvancesRebootingNodeToRunning(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-rebooting"] = docker.ObservedContainer{ID: "c-rebooting", State: "running", Healthy: true}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNodeWithTarget(t, st, "wt-op", "n-rebooting", "c-rebooting", StateRebooting)

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	advanced, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: node.ID, Namespace: "wt-op"})
	if err != nil {
		t.Fatalf("load advanced node: %v", err)
	}
	if advanced.TargetState != StateRunning {
		t.Fatalf("target = %q, want %q", advanced.TargetState, StateRunning)
	}
}

func TestTickAdvancesStoppingNodeToStopped(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-stopping"] = docker.ObservedContainer{ID: "c-stopping", State: "exited"}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNodeWithTarget(t, st, "wt-op", "n-stopping", "c-stopping", StateStopping)

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	advanced, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: node.ID, Namespace: "wt-op"})
	if err != nil {
		t.Fatalf("load advanced node: %v", err)
	}
	if advanced.TargetState != StateStopped {
		t.Fatalf("target = %q, want %q", advanced.TargetState, StateStopped)
	}
	// The next pass derives the display from the settled target.
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	settled, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: node.ID, Namespace: "wt-op"})
	if err != nil {
		t.Fatalf("reload settled node: %v", err)
	}
	if settled.ObservedState != StateStopped {
		t.Fatalf("observed = %q, want %q", settled.ObservedState, StateStopped)
	}
}

func TestTickLeavesRunningNodeWithExitedContainer(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-exited"] = docker.ObservedContainer{ID: "c-exited", State: "exited"}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNodeWithTarget(t, st, "wt-op", "n-exited", "c-exited", StateRunning)

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	refreshed, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: node.ID, Namespace: "wt-op"})
	if err != nil {
		t.Fatalf("load refreshed node: %v", err)
	}
	if refreshed.TargetState != StateRunning {
		t.Fatalf("target = %q, want unchanged %q", refreshed.TargetState, StateRunning)
	}
	if refreshed.ObservedState != StateStopped {
		t.Fatalf("observed = %q, want %q", refreshed.ObservedState, StateStopped)
	}
}

func TestTickDefersDeleteOfLaunchingNode(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-launching"] = docker.ObservedContainer{ID: "c-launching", State: "running", Healthy: true}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNodeWithTarget(t, st, "wt-op", "n-launching", "c-launching", StateLaunching)
	if _, err := st.Queries().CreateFleetNodeCredential(context.Background(), db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: "hash-launching",
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	op := seedCoordinatorOperation(t, st, node.ID, "delete", OpPhaseApproved, "delete-launching")

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(provider.removed) != 0 || len(provider.removedVolumes) != 0 {
		t.Fatalf("launching node reached delete: containers %v volumes %v", provider.removed, provider.removedVolumes)
	}
	if got := loadCoordinatorOperation(t, st, op.ID).Phase; got != OpPhaseApproved {
		t.Fatalf("phase = %q, want untouched %q", got, OpPhaseApproved)
	}
	still, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: node.ID, Namespace: "wt-op"})
	if err != nil {
		t.Fatalf("load node: %v", err)
	}
	if still.TargetState != StateLaunching {
		t.Fatalf("target = %q, want %q", still.TargetState, StateLaunching)
	}
	if _, err := st.Queries().GetFleetNodeCredentialByHash(context.Background(), "hash-launching"); err != nil {
		t.Fatalf("deferred delete revoked the credential: %v", err)
	}
}

func TestTickExecutesApprovedStartAndReboot(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-start"] = docker.ObservedContainer{ID: "c-start", State: "exited"}
	provider.inspected["c-reboot"] = docker.ObservedContainer{ID: "c-reboot", State: "running", Healthy: true}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	startNode := seedCoordinatorNodeWithTarget(t, st, "wt-op", "n-start", "c-start", StateStopped)
	rebootNode := seedCoordinatorNodeWithTarget(t, st, "wt-op", "n-reboot", "c-reboot", StateRunning)
	startOp := seedCoordinatorOperation(t, st, startNode.ID, "start", OpPhaseApproved, "start-1")
	rebootOp := seedCoordinatorOperation(t, st, rebootNode.ID, "reboot", OpPhaseApproved, "reboot-1")

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(provider.started) != 1 || provider.started[0] != "c-start" {
		t.Fatalf("started = %v", provider.started)
	}
	if len(provider.restarted) != 1 || provider.restarted[0] != "c-reboot" {
		t.Fatalf("restarted = %v", provider.restarted)
	}
	if got := loadCoordinatorOperation(t, st, startOp.ID).Phase; got != OpPhaseDone {
		t.Fatalf("start phase = %q, want %q", got, OpPhaseDone)
	}
	if got := loadCoordinatorOperation(t, st, rebootOp.ID).Phase; got != OpPhaseDone {
		t.Fatalf("reboot phase = %q, want %q", got, OpPhaseDone)
	}
}

func TestClaimOperationHasSingleWinner(t *testing.T) {
	provider := newFakeDocker()
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNode(t, st, "wt-op", "n-claim", "c-claim")
	op := seedCoordinatorOperation(t, st, node.ID, "stop", OpPhaseApproved, "stop-claim")

	claimed, err := svc.claimOperation(context.Background(), op)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed {
		t.Fatal("first claim lost the race")
	}
	// The second caller still holds the approved snapshot; its conditional
	// update must not match the executing row.
	claimedAgain, err := svc.claimOperation(context.Background(), op)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimedAgain {
		t.Fatal("second claim matched a row another coordinator owns")
	}
	if got := loadCoordinatorOperation(t, st, op.ID).Phase; got != OpPhaseExecuting {
		t.Fatalf("phase = %q, want %q", got, OpPhaseExecuting)
	}
}

func TestTickRecoversTimedOutExecutingOperation(t *testing.T) {
	ctx := context.Background()
	pool := serviceTestPool(t)
	st := store.New(pool)
	provider := newFakeDocker()
	provider.inspected["c-timeout"] = docker.ObservedContainer{ID: "c-timeout", State: "running", Healthy: true}
	svc := NewService(Config{Namespace: "wt-op"}, st, provider, nil, "secret")
	node := seedCoordinatorNode(t, st, "wt-op", "n-timeout", "c-timeout")
	op := seedCoordinatorOperation(t, st, node.ID, "stop", OpPhaseExecuting, "stop-timeout")
	if _, err := pool.Exec(ctx,
		"UPDATE fleet_node_operation SET updated_at = now() - interval '2 minutes' WHERE id = $1",
		op.ID); err != nil {
		t.Fatalf("age operation: %v", err)
	}

	if err := svc.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(provider.stopped) != 1 || provider.stopped[0] != "c-timeout" {
		t.Fatalf("timed-out operation was not re-executed: %v", provider.stopped)
	}
	if got := loadCoordinatorOperation(t, st, op.ID).Phase; got != OpPhaseDone {
		t.Fatalf("phase = %q, want %q", got, OpPhaseDone)
	}
}

func TestTickLeavesFreshExecutingOperationAlone(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-executing"] = docker.ObservedContainer{ID: "c-executing", State: "running", Healthy: true}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNode(t, st, "wt-op", "n-executing", "c-executing")
	op := seedCoordinatorOperation(t, st, node.ID, "stop", OpPhaseExecuting, "stop-executing")

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(provider.stopped) != 0 {
		t.Fatalf("fresh executing operation was re-executed: %v", provider.stopped)
	}
	if got := loadCoordinatorOperation(t, st, op.ID).Phase; got != OpPhaseExecuting {
		t.Fatalf("phase = %q, want untouched %q", got, OpPhaseExecuting)
	}
}

func TestTickFailsOperationAfterMaxRetries(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-retry"] = docker.ObservedContainer{ID: "c-retry", State: "running", Healthy: true}
	provider.stopErr = errFakeStop
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNode(t, st, "wt-op", "n-retry", "c-retry")
	op := seedCoordinatorOperation(t, st, node.ID, "stop", OpPhaseApproved, "stop-retry")

	for attempt := 1; attempt <= maxOperationRetries; attempt++ {
		if err := svc.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", attempt, err)
		}
		got := loadCoordinatorOperation(t, st, op.ID)
		if got.RetryCount != int32(attempt) {
			t.Fatalf("after attempt %d retry_count = %d", attempt, got.RetryCount)
		}
		if attempt < maxOperationRetries && got.Phase != OpPhaseApproved {
			t.Fatalf("after attempt %d phase = %q, want %q", attempt, got.Phase, OpPhaseApproved)
		}
		if attempt == maxOperationRetries {
			if got.Phase != OpPhaseFailed {
				t.Fatalf("phase = %q, want %q after %d attempts", got.Phase, OpPhaseFailed, attempt)
			}
			if !strings.Contains(got.Error, errFakeStop.Error()) {
				t.Fatalf("error = %q, want the Docker error", got.Error)
			}
		}
	}
	// A parked failure is not picked up again.
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("post-failure tick: %v", err)
	}
	if got := loadCoordinatorOperation(t, st, op.ID); got.RetryCount != int32(maxOperationRetries) {
		t.Fatalf("retry_count moved after the operation failed: %d", got.RetryCount)
	}
}

func TestMarkOperationPhaseKeepsPreviousError(t *testing.T) {
	ctx := context.Background()
	pool := serviceTestPool(t)
	st := store.New(pool)
	svc := NewService(Config{Namespace: "wt-op"}, st, newFakeDocker(), nil, "secret")
	node := seedCoordinatorNode(t, st, "wt-op", "n-keep-error", "c-keep-error")
	op := seedCoordinatorOperation(t, st, node.ID, "stop", OpPhaseApproved, "stop-keep-error")
	if _, err := pool.Exec(ctx,
		"UPDATE fleet_node_operation SET error = 'previous failure' WHERE id = $1", op.ID); err != nil {
		t.Fatalf("seed previous error: %v", err)
	}

	claimed, err := svc.claimOperation(ctx, op)
	if err != nil || !claimed {
		t.Fatalf("claim = %v/%v", claimed, err)
	}
	if got := loadCoordinatorOperation(t, st, op.ID).Error; got != "previous failure" {
		t.Fatalf("error = %q, want the previous message preserved on claim", got)
	}
	if err := svc.completeOperation(ctx, op); err != nil {
		t.Fatalf("complete: %v", err)
	}
	done := loadCoordinatorOperation(t, st, op.ID)
	if done.Phase != OpPhaseDone {
		t.Fatalf("phase = %q, want %q", done.Phase, OpPhaseDone)
	}
	if done.Error != "previous failure" {
		t.Fatalf("error = %q, want the previous message preserved on completion", done.Error)
	}
}

func TestTickRefreshesOnlyOwnNamespaceNodes(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-own-ns"] = docker.ObservedContainer{ID: "c-own-ns", State: "running", Healthy: true}
	provider.inspected["c-foreign-ns"] = docker.ObservedContainer{ID: "c-foreign-ns", State: "running", Healthy: true}
	svc, st := coordinatorTestService(t, "wt-op", provider)
	own := seedCoordinatorNode(t, st, "wt-op", "own-ns", "c-own-ns")
	foreign := seedCoordinatorNode(t, st, "other-ns", "foreign-ns", "c-foreign-ns")

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	inspects := 0
	for _, call := range provider.calls {
		if call == "inspect" {
			inspects++
		}
	}
	if inspects != 1 {
		t.Fatalf("inspect calls = %d, want only the namespace node", inspects)
	}
	refreshed, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: own.ID, Namespace: "wt-op"})
	if err != nil {
		t.Fatalf("load own node: %v", err)
	}
	if refreshed.ObservedState != StateRunning {
		t.Fatalf("own observed = %q, want %q", refreshed.ObservedState, StateRunning)
	}
	untouched, err := st.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{ID: foreign.ID, Namespace: "other-ns"})
	if err != nil {
		t.Fatalf("load foreign node: %v", err)
	}
	if untouched.ObservedState != "" || untouched.DaemonReady {
		t.Fatalf("foreign node was refreshed: observed %q ready %v", untouched.ObservedState, untouched.DaemonReady)
	}
}
