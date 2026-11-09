package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cloudruntime"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const localFleetTestNamespace = "handler-test-fleet"

type localFleetTestFixture struct {
	NodeID      string
	RuntimeID   string
	AgentID     string
	DaemonID    string
	ContainerID string
}

// newLocalFleetTestCoordinator builds the API-side coordinator against the
// suite's database. The diagnose callback is injected so these tests exercise
// scheduling semantics without Docker or Fleet HTTP.
func newLocalFleetTestCoordinator(t *testing.T, pending int) *localFleetCoordinator {
	t.Helper()
	requireLocalFleetTables(t)
	return &localFleetCoordinator{
		pool:      testPool,
		namespace: localFleetTestNamespace,
		diagnose: func(context.Context, string, string) (int, error) {
			return pending, nil
		},
		now: time.Now,
	}
}

func requireLocalFleetTables(t *testing.T) {
	t.Helper()
	if testPool == nil {
		t.Skip("database is not configured")
	}
	var exists bool
	if err := testPool.QueryRow(context.Background(), `
		SELECT to_regclass('fleet_node') IS NOT NULL
		   AND to_regclass('fleet_node_operation') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check fleet tables: %v", err)
	}
	if !exists {
		t.Skip("fleet tables are not migrated in this database")
	}
}

// localFleetTestSuffix keeps fixture and idempotency-key names unique across
// tests and repeated runs. fleet_node_operation has a global unique index on
// idempotency_key, so fixed keys collide as soon as one row survives.
func localFleetTestSuffix(t *testing.T) string {
	t.Helper()
	return strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" +
		strconv.FormatInt(time.Now().UnixNano(), 36)
}

func localFleetTestKey(t *testing.T, base string) string {
	t.Helper()
	return base + "-" + localFleetTestSuffix(t)
}

// seedLocalFleetTestNode inserts a runtime, agent, and node for the test. The
// operation rows created by the coordinator are cleaned up before the node
// itself so the unique idempotency index never leaks between tests.
func seedLocalFleetTestNode(t *testing.T, targetState string, over ...testutil.Cols) localFleetTestFixture {
	t.Helper()
	requireLocalFleetTables(t)

	suffix := localFleetTestSuffix(t)
	daemonID := "daemon-" + suffix
	containerID := "container-" + suffix

	runtimeID := dbfx.Runtime(t, "Fleet test runtime "+suffix, testutil.Cols{
		"daemon_id":    daemonID,
		"runtime_mode": "local",
		"provider":     "local_fleet_test",
		"owner_id":     testUserID,
	})
	agentID := dbfx.Agent(t, "Fleet test agent "+suffix, runtimeID, testutil.Cols{
		"runtime_mode": "local",
	})
	cols := testutil.Cols{
		"owner_id":          testUserID,
		"namespace":         localFleetTestNamespace,
		"name":              "fleet-node-" + suffix,
		"container_id":      containerID,
		"container_name":    "fleet-node-" + suffix,
		"daemon_id":         daemonID,
		"image":             "multica-runtime:test",
		"spec":              testutil.Raw("'{}'::jsonb"),
		"target_state":      targetState,
		"observed_state":    targetState,
		"daemon_ready":      true,
		"health_checked_at": testutil.Raw("now()"),
	}
	for _, override := range over {
		for key, value := range override {
			cols[key] = value
		}
	}
	nodeID := dbfx.Insert(t, "fleet_node", cols)
	dbfx.Cleanup(t, `DELETE FROM fleet_node_operation WHERE node_id = $1`, nodeID)

	return localFleetTestFixture{
		NodeID:      nodeID,
		RuntimeID:   runtimeID,
		AgentID:     agentID,
		DaemonID:    daemonID,
		ContainerID: containerID,
	}
}

// seedOtherWorkspaceTask queues work for the same owner and daemon in a second
// workspace. Busy checks are owner + daemon scoped, so this must block too.
func seedOtherWorkspaceTask(t *testing.T, fx localFleetTestFixture, status string) string {
	t.Helper()
	suffix := localFleetTestSuffix(t)
	workspaceID := dbfx.Workspace(t, "Fleet cross workspace "+suffix, "fleet-cross-"+suffix)
	runtimeID := dbfx.Runtime(t, "Fleet cross runtime "+suffix, testutil.Cols{
		"workspace_id": workspaceID,
		"daemon_id":    fx.DaemonID,
		"owner_id":     testUserID,
	})
	agentID := dbfx.Agent(t, "Fleet cross agent "+suffix, runtimeID, testutil.Cols{
		"workspace_id": workspaceID,
	})
	return dbfx.Task(t, agentID, testutil.Cols{"status": status})
}

func localFleetNodeTargetState(t *testing.T, nodeID string) string {
	t.Helper()
	var state string
	dbfx.QueryRow(t, `SELECT target_state FROM fleet_node WHERE id = $1`, nodeID).Scan(&state)
	return state
}

func localFleetOperationCount(t *testing.T, nodeID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM fleet_node_operation WHERE node_id = $1`, nodeID)
}

func localFleetPhaseCount(t *testing.T, nodeID, phase string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM fleet_node_operation
		WHERE node_id = $1 AND phase = $2`, nodeID, phase)
}

func TestPrepareStopRejectsActiveButAllowsQueued(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	taskID := dbfx.Task(t, fx.AgentID, testutil.Cols{"status": "running"})

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", localFleetTestKey(t, "stop-running")); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("running task: err = %v, want ErrNodeBusy", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateRunning {
		t.Fatalf("rejected prepare changed target_state to %q", got)
	}
	if got := localFleetOperationCount(t, fx.NodeID); got != 0 {
		t.Fatalf("rejected prepare committed %d operations", got)
	}

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'queued' WHERE id = $1`, taskID)
	node, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", localFleetTestKey(t, "stop-queued"))
	if err != nil {
		t.Fatalf("queued task must not block stop: %v", err)
	}
	if node.TargetState != localFleetStateStopping {
		t.Fatalf("target_state = %q, want %q", node.TargetState, localFleetStateStopping)
	}
}

func TestPrepareDeleteRejectsQueuedRuns(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	dbfx.Task(t, fx.AgentID, testutil.Cols{"status": "queued"})

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "delete", localFleetTestKey(t, "delete-queued")); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("delete with queued run: err = %v, want ErrNodeBusy", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateRunning {
		t.Fatalf("rejected delete changed target_state to %q", got)
	}
	if got := localFleetOperationCount(t, fx.NodeID); got != 0 {
		t.Fatalf("rejected delete committed %d operations", got)
	}
}

func TestPrepareDeleteRejectsActiveRuns(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	dbfx.Task(t, fx.AgentID, testutil.Cols{"status": "dispatched"})

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "delete", localFleetTestKey(t, "delete-active")); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("delete with dispatched run: err = %v, want ErrNodeBusy", err)
	}
}

func TestPrepareDeleteRejectsLaunchingNode(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateLaunching)

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "delete", localFleetTestKey(t, "delete-launching")); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("delete on launching node: err = %v, want ErrNodeBusy", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateLaunching {
		t.Fatalf("rejected delete changed target_state to %q", got)
	}
}

func TestPrepareRejectsTerminatedNode(t *testing.T) {
	for _, operation := range []string{"stop", "reboot", "delete"} {
		t.Run(operation, func(t *testing.T) {
			coord := newLocalFleetTestCoordinator(t, 0)
			fx := seedLocalFleetTestNode(t, localFleetStateTerminated)
			if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, operation, localFleetTestKey(t, "terminated")); !errors.Is(err, ErrOperationConflict) {
				t.Fatalf("%s on terminated node: err = %v, want ErrOperationConflict", operation, err)
			}
			if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateTerminated {
				t.Fatalf("rejected %s changed target_state to %q", operation, got)
			}
		})
	}
}

func TestPrepareFailedNodeOnlyAcceptsDelete(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateFailed, testutil.Cols{
		"container_id":   "",
		"observed_state": localFleetStateInstanceMissing,
	})

	for _, operation := range []string{"stop", "reboot"} {
		if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, operation, localFleetTestKey(t, "failed-"+operation)); !errors.Is(err, ErrOperationConflict) {
			t.Fatalf("%s on failed node: err = %v, want ErrOperationConflict", operation, err)
		}
	}
	// Delete is the one operation a failed launch still accepts: there is no
	// container, so the daemon diagnosis is skipped instead of 503ing forever.
	coord.diagnose = func(context.Context, string, string) (int, error) {
		t.Fatal("diagnose must not run for a node without a container")
		return 0, nil
	}
	node, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{NodeID: fx.NodeID}, "delete", localFleetTestKey(t, "failed-delete"))
	if err != nil {
		t.Fatalf("delete on failed node: %v", err)
	}
	if node.TargetState != localFleetStateTerminating {
		t.Fatalf("target_state = %q, want %q", node.TargetState, localFleetStateTerminating)
	}
}

func TestPrepareStopRejectsNodeWithoutContainer(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning, testutil.Cols{
		"container_id":   "",
		"observed_state": localFleetStateRunning,
	})

	for _, operation := range []string{"stop", "reboot"} {
		if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{NodeID: fx.NodeID}, operation, localFleetTestKey(t, "no-container-"+operation)); !errors.Is(err, ErrOperationConflict) {
			t.Fatalf("%s without container: err = %v, want ErrOperationConflict", operation, err)
		}
	}
}

func TestPrepareDeleteAfterStopSkipsDiagnose(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning, testutil.Cols{
		"observed_state": localFleetStateStopped,
	})
	coord.diagnose = func(context.Context, string, string) (int, error) {
		t.Fatal("diagnose must not run for a stopped container")
		return 0, nil
	}

	node, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "delete", localFleetTestKey(t, "stopped-delete"))
	if err != nil {
		t.Fatalf("delete after stop: %v", err)
	}
	if node.TargetState != localFleetStateTerminating {
		t.Fatalf("target_state = %q, want %q", node.TargetState, localFleetStateTerminating)
	}
}

func TestPrepareIsIdempotentForSameRequest(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	key := localFleetTestKey(t, "idem")

	first, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", key)
	if err != nil {
		t.Fatalf("approved replay must return the node: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("same key must return the same node: %v vs %v", first.ID, second.ID)
	}
	if second.TargetState != localFleetStateStopping {
		t.Fatalf("target_state = %q, want %q", second.TargetState, localFleetStateStopping)
	}
	if got := localFleetOperationCount(t, fx.NodeID); got != 1 {
		t.Fatalf("operation count = %d, want 1", got)
	}

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "reboot", key); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("different operation with same key: err = %v, want ErrOperationConflict", err)
	}
}

func TestPrepareRejectsSecondPendingOperation(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", localFleetTestKey(t, "first-op")); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "reboot", localFleetTestKey(t, "second-op")); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("second pending operation: err = %v, want ErrOperationConflict", err)
	}
	if got := localFleetOperationCount(t, fx.NodeID); got != 1 {
		t.Fatalf("operation count = %d, want 1", got)
	}
}

func TestPrepareAbortsPendingTerminalReports(t *testing.T) {
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	coord := newLocalFleetTestCoordinator(t, 1)

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", localFleetTestKey(t, "pending-key")); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("pending terminal reports: err = %v, want ErrNodeBusy", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateRunning {
		t.Fatalf("aborted prepare left target_state = %q, want %q", got, localFleetStateRunning)
	}
	if got := localFleetPhaseCount(t, fx.NodeID, localFleetOpAborted); got != 1 {
		t.Fatalf("aborted operations = %d, want 1", got)
	}

	clear := newLocalFleetTestCoordinator(t, 0)
	node, err := clear.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", localFleetTestKey(t, "pending-key-2"))
	if err != nil {
		t.Fatalf("aborted prepare must clear the barrier: %v", err)
	}
	if node.TargetState != localFleetStateStopping {
		t.Fatalf("target_state = %q, want %q", node.TargetState, localFleetStateStopping)
	}
}

func TestPrepareKeepsBarrierWhenDiagnoseUncertain(t *testing.T) {
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	uncertain := newLocalFleetTestCoordinator(t, 0)
	uncertain.diagnose = func(context.Context, string, string) (int, error) {
		return 0, fmt.Errorf("%w: read daemon health: timeout", errLocalFleetStateUncertain)
	}
	key := localFleetTestKey(t, "uncertain")

	_, err := uncertain.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", key)
	if err == nil {
		t.Fatal("unreadable running daemon must reject the operation")
	}
	if !errors.Is(err, errLocalFleetDiagnoseUnavailable) || !errors.Is(err, errLocalFleetStateUncertain) {
		t.Fatalf("err = %v, want diagnose unavailable + state uncertain", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateStopping {
		t.Fatalf("target_state = %q, want %q (barrier stays prepared)", got, localFleetStateStopping)
	}
	if got := localFleetPhaseCount(t, fx.NodeID, localFleetOpPrepared); got != 1 {
		t.Fatalf("prepared operations = %d, want 1", got)
	}

	healthy := newLocalFleetTestCoordinator(t, 0)
	node, err := healthy.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", key)
	if err != nil {
		t.Fatalf("same-key retry must resume the prepared operation: %v", err)
	}
	if node.TargetState != localFleetStateStopping {
		t.Fatalf("target_state = %q, want %q", node.TargetState, localFleetStateStopping)
	}
	if got := localFleetPhaseCount(t, fx.NodeID, "approved"); got != 1 {
		t.Fatalf("approved operations = %d, want 1", got)
	}
}

func TestPrepareAbortsWhenDiagnoseUnavailable(t *testing.T) {
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	coord := newLocalFleetTestCoordinator(t, 0)
	coord.diagnose = func(context.Context, string, string) (int, error) {
		return 0, errors.New("fleet unreachable")
	}

	_, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", localFleetTestKey(t, "diagnose-failure"))
	if err == nil {
		t.Fatal("unreadable diagnose must reject the operation")
	}
	if errors.Is(err, ErrNodeBusy) || errors.Is(err, ErrOperationConflict) || errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("diagnose failure = %v, want a transport-style error", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateRunning {
		t.Fatalf("target_state = %q, want %q (prior target restored)", got, localFleetStateRunning)
	}
	if got := localFleetPhaseCount(t, fx.NodeID, localFleetOpAborted); got != 1 {
		t.Fatalf("aborted operations = %d, want 1", got)
	}

	healthy := newLocalFleetTestCoordinator(t, 0)
	node, err := healthy.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", localFleetTestKey(t, "diagnose-retry"))
	if err != nil {
		t.Fatalf("aborted barrier must be retryable: %v", err)
	}
	if node.TargetState != localFleetStateStopping {
		t.Fatalf("target_state = %q, want %q", node.TargetState, localFleetStateStopping)
	}
}

func TestPrepareRechecksBusyAfterDiagnose(t *testing.T) {
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	coord := newLocalFleetTestCoordinator(t, 0)
	coord.diagnose = func(context.Context, string, string) (int, error) {
		// Work arrives while prepare holds no transaction lock.
		dbfx.Task(t, fx.AgentID, testutil.Cols{"status": "running"})
		return 0, nil
	}

	_, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", localFleetTestKey(t, "late-task"))
	if !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("task arriving after prepare: err = %v, want ErrNodeBusy", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateRunning {
		t.Fatalf("target_state = %q, want %q", got, localFleetStateRunning)
	}
	if got := localFleetPhaseCount(t, fx.NodeID, localFleetOpAborted); got != 1 {
		t.Fatalf("aborted operations = %d, want 1", got)
	}
}

func TestPrepareAbortsWhenGenerationChangesAfterDiagnose(t *testing.T) {
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	coord := newLocalFleetTestCoordinator(t, 0)
	coord.diagnose = func(context.Context, string, string) (int, error) {
		// Simulate another actor settling the node between the two transactions.
		dbfx.Exec(t, `UPDATE fleet_node SET generation = generation + 1 WHERE id = $1`, fx.NodeID)
		return 0, nil
	}

	_, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", localFleetTestKey(t, "generation-change"))
	if !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("generation change: err = %v, want ErrOperationConflict", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateRunning {
		t.Fatalf("target_state = %q, want %q (prior target restored)", got, localFleetStateRunning)
	}
	if got := localFleetPhaseCount(t, fx.NodeID, localFleetOpAborted); got != 1 {
		t.Fatalf("aborted operations = %d, want 1", got)
	}
}

func TestPrepareFinishReturnsNodeWhenApprovedConcurrently(t *testing.T) {
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	coord := newLocalFleetTestCoordinator(t, 0)
	coord.diagnose = func(context.Context, string, string) (int, error) {
		// Simulate a concurrent retry of the same request approving first.
		dbfx.Exec(t, `UPDATE fleet_node_operation
			SET phase = 'approved', approved_at = now()
			WHERE node_id = $1 AND phase = 'prepared'`, fx.NodeID)
		return 0, nil
	}

	node, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", localFleetTestKey(t, "concurrent-approve"))
	if err != nil {
		t.Fatalf("already-approved operation must replay the node, got %v", err)
	}
	if node.TargetState != localFleetStateStopping {
		t.Fatalf("target_state = %q, want %q", node.TargetState, localFleetStateStopping)
	}
	if got := localFleetPhaseCount(t, fx.NodeID, "approved"); got != 1 {
		t.Fatalf("approved operations = %d, want 1", got)
	}
}

func TestPrepareBusyChecksCoverOtherWorkspaces(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	seedOtherWorkspaceTask(t, fx, "running")

	for _, operation := range []string{"stop", "delete"} {
		if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, operation, localFleetTestKey(t, "cross-workspace-"+operation)); !errors.Is(err, ErrNodeBusy) {
			t.Fatalf("%s with a task in another workspace: err = %v, want ErrNodeBusy", operation, err)
		}
	}
	if got := localFleetOperationCount(t, fx.NodeID); got != 0 {
		t.Fatalf("blocked operations = %d, want 0", got)
	}
}

func TestPrepareDeleteRejectsQueuedTaskInOtherWorkspace(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	seedOtherWorkspaceTask(t, fx, "queued")

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "delete", localFleetTestKey(t, "cross-workspace-queued")); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("delete with queued task in another workspace: err = %v, want ErrNodeBusy", err)
	}
}

func TestPrepareRejectsInvalidNodeID(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{NodeID: "not-a-uuid"}, "stop", localFleetTestKey(t, "invalid-node")); !errors.Is(err, errLocalFleetInvalidNodeID) {
		t.Fatalf("invalid node id: err = %v, want errLocalFleetInvalidNodeID", err)
	}
}

type localFleetStubProxy struct {
	calls int
}

func (s *localFleetStubProxy) Enabled() bool { return true }

func (s *localFleetStubProxy) Do(context.Context, cloudruntime.Request) (*cloudruntime.Response, error) {
	s.calls++
	return &cloudruntime.Response{StatusCode: http.StatusOK, Body: []byte("{}")}, nil
}

func TestLocalFleetProxyRequiresIdempotencyKey(t *testing.T) {
	proxy := &localFleetStubProxy{}
	h := &Handler{
		CloudRuntime: proxy,
		LocalFleet:   &localFleetCoordinator{now: time.Now},
	}
	req := newRequest(http.MethodDelete, "/api/cloud-runtime/nodes", map[string]any{"instance_id": "container-1"})
	rec := httptest.NewRecorder()

	h.DeleteCloudRuntimeNode(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if proxy.calls != 0 {
		t.Fatalf("local mode forwarded to Fleet %d times", proxy.calls)
	}
}

func TestWriteLocalFleetErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"not found", fmt.Errorf("load: %w", ErrNodeNotFound), http.StatusNotFound, "not_found"},
		{"busy", ErrNodeBusy, http.StatusConflict, "node_busy"},
		{"conflict", ErrOperationConflict, http.StatusConflict, "conflict"},
		{"invalid node id", errLocalFleetInvalidNodeID, http.StatusBadRequest, "invalid_request"},
		{"diagnose unavailable", fmt.Errorf("%w: timeout", errLocalFleetDiagnoseUnavailable), http.StatusServiceUnavailable, "local_fleet_unavailable"},
		{"state uncertain", fmt.Errorf("%w: %w", errLocalFleetDiagnoseUnavailable, errLocalFleetStateUncertain), http.StatusServiceUnavailable, "state_uncertain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeLocalFleetError(rec, httptest.NewRequest(http.MethodPost, "/", nil), tc.err)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Code != tc.code {
				t.Fatalf("code = %q, want %q", body.Code, tc.code)
			}
		})
	}
}
