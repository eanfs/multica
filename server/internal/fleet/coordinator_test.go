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
	return seedServiceNodeWith(t, st, db.CreateFleetNodeParams{
		OwnerID:       testOwner(60),
		Namespace:     namespace,
		Name:          name,
		ContainerID:   containerID,
		ContainerName: name + "-container",
		DaemonID:      name + "-daemon",
		Image:         "multica/runtime-claude:test",
		Spec:          []byte("{\"id\":\"standard\"}"),
		TargetState:   StateRunning,
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

func TestTickRecordsFailedOperation(t *testing.T) {
	provider := newFakeDocker()
	provider.inspected["c-fail"] = docker.ObservedContainer{ID: "c-fail", State: "running", Healthy: true}
	provider.stopErr = errFakeStop
	svc, st := coordinatorTestService(t, "wt-op", provider)
	node := seedCoordinatorNode(t, st, "wt-op", "n-fail", "c-fail")
	op := seedCoordinatorOperation(t, st, node.ID, "stop", OpPhaseApproved, "stop-fail")

	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick must not abort on one failed operation: %v", err)
	}
	failed := loadCoordinatorOperation(t, st, op.ID)
	if failed.Phase != OpPhaseFailed {
		t.Fatalf("phase = %q, want %q", failed.Phase, OpPhaseFailed)
	}
	if failed.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", failed.RetryCount)
	}
	if !strings.Contains(failed.Error, errFakeStop.Error()) {
		t.Fatalf("error = %q, want the Docker error", failed.Error)
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
