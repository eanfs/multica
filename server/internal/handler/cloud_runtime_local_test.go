package handler

import (
	"context"
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

func seedLocalFleetTestNode(t *testing.T, targetState string) localFleetTestFixture {
	t.Helper()
	requireLocalFleetTables(t)

	suffix := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" +
		strconv.FormatInt(time.Now().UnixNano(), 36)
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
	nodeID := dbfx.Insert(t, "fleet_node", testutil.Cols{
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
	})

	return localFleetTestFixture{
		NodeID:      nodeID,
		RuntimeID:   runtimeID,
		AgentID:     agentID,
		DaemonID:    daemonID,
		ContainerID: containerID,
	}
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

func TestPrepareStopRejectsActiveButAllowsQueued(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	taskID := dbfx.Task(t, fx.AgentID, testutil.Cols{"status": "running"})

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", "stop-running"); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("running task: err = %v, want ErrNodeBusy", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateRunning {
		t.Fatalf("rejected prepare changed target_state to %q", got)
	}
	if got := localFleetOperationCount(t, fx.NodeID); got != 0 {
		t.Fatalf("rejected prepare committed %d operations", got)
	}

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'queued' WHERE id = $1`, taskID)
	node, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", "stop-queued")
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

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "delete", "delete-queued"); !errors.Is(err, ErrNodeBusy) {
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

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "delete", "delete-active"); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("delete with dispatched run: err = %v, want ErrNodeBusy", err)
	}
}

func TestPrepareDeleteRejectsLaunchingNode(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateLaunching)

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "delete", "delete-launching"); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("delete on launching node: err = %v, want ErrNodeBusy", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateLaunching {
		t.Fatalf("rejected delete changed target_state to %q", got)
	}
}

func TestPrepareIsIdempotentForSameRequest(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)

	first, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", "idem-key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", "idem-key")
	if err != nil {
		t.Fatal(err)
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

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "reboot", "idem-key"); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("different operation with same key: err = %v, want ErrOperationConflict", err)
	}
}

func TestPrepareRejectsSecondPendingOperation(t *testing.T) {
	coord := newLocalFleetTestCoordinator(t, 0)
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", "first-op"); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "reboot", "second-op"); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("second pending operation: err = %v, want ErrOperationConflict", err)
	}
	if got := localFleetOperationCount(t, fx.NodeID); got != 1 {
		t.Fatalf("operation count = %d, want 1", got)
	}
}

func TestPrepareAbortsPendingTerminalReports(t *testing.T) {
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	coord := newLocalFleetTestCoordinator(t, 1)

	if _, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", "pending-key"); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("pending terminal reports: err = %v, want ErrNodeBusy", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateRunning {
		t.Fatalf("aborted prepare left target_state = %q, want %q", got, localFleetStateRunning)
	}
	aborted := dbfx.Count(t, `SELECT count(*) FROM fleet_node_operation
		WHERE node_id = $1 AND phase = 'aborted' AND approved_at IS NULL`, fx.NodeID)
	if aborted != 1 {
		t.Fatalf("aborted operations = %d, want 1", aborted)
	}

	clear := newLocalFleetTestCoordinator(t, 0)
	node, err := clear.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", "pending-key-2")
	if err != nil {
		t.Fatalf("aborted prepare must clear the barrier: %v", err)
	}
	if node.TargetState != localFleetStateStopping {
		t.Fatalf("target_state = %q, want %q", node.TargetState, localFleetStateStopping)
	}
}

func TestPrepareReauthorizesPreparedOperation(t *testing.T) {
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	failing := newLocalFleetTestCoordinator(t, 0)
	failing.diagnose = func(context.Context, string, string) (int, error) {
		return 0, errors.New("fleet unreachable")
	}
	if _, err := failing.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", "resume-key"); err == nil {
		t.Fatal("unreadable diagnose must fail the first attempt")
	}
	prepared := dbfx.Count(t, `SELECT count(*) FROM fleet_node_operation
		WHERE node_id = $1 AND phase = 'prepared'`, fx.NodeID)
	if prepared != 1 {
		t.Fatalf("prepared operations = %d, want 1", prepared)
	}

	healthy := newLocalFleetTestCoordinator(t, 0)
	node, err := healthy.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", "resume-key")
	if err != nil {
		t.Fatalf("same-key retry must resume the prepared operation: %v", err)
	}
	if node.TargetState != localFleetStateStopping {
		t.Fatalf("target_state = %q, want %q", node.TargetState, localFleetStateStopping)
	}
	approved := dbfx.Count(t, `SELECT count(*) FROM fleet_node_operation
		WHERE node_id = $1 AND phase = 'approved' AND approved_at IS NOT NULL`, fx.NodeID)
	if approved != 1 {
		t.Fatalf("approved operations = %d, want 1", approved)
	}
}

func TestPrepareKeepsBarrierWhenDiagnoseFails(t *testing.T) {
	fx := seedLocalFleetTestNode(t, localFleetStateRunning)
	coord := newLocalFleetTestCoordinator(t, 0)
	coord.diagnose = func(context.Context, string, string) (int, error) {
		return 0, errors.New("fleet unreachable")
	}

	_, err := coord.Prepare(context.Background(), testUserID, localFleetNodeRef{InstanceID: fx.ContainerID}, "stop", "diagnose-failure")
	if err == nil {
		t.Fatal("unreadable diagnose must reject the operation")
	}
	if errors.Is(err, ErrNodeBusy) || errors.Is(err, ErrOperationConflict) || errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("diagnose failure = %v, want a transport-style error", err)
	}
	if got := localFleetNodeTargetState(t, fx.NodeID); got != localFleetStateStopping {
		t.Fatalf("target_state = %q, want %q (barrier stays prepared)", got, localFleetStateStopping)
	}
	prepared := dbfx.Count(t, `SELECT count(*) FROM fleet_node_operation
		WHERE node_id = $1 AND phase = 'prepared'`, fx.NodeID)
	if prepared != 1 {
		t.Fatalf("prepared operations = %d, want 1", prepared)
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
	}{
		{"not found", fmt.Errorf("load: %w", ErrNodeNotFound), http.StatusNotFound},
		{"busy", ErrNodeBusy, http.StatusConflict},
		{"conflict", ErrOperationConflict, http.StatusConflict},
		{"diagnose unavailable", fmt.Errorf("%w: timeout", errLocalFleetDiagnoseUnavailable), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeLocalFleetError(rec, httptest.NewRequest(http.MethodPost, "/", nil), tc.err)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
