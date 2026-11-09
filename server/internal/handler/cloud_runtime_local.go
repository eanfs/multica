package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/cloudruntime"
	"github.com/multica-ai/multica/server/internal/fleet/barrier"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Local Fleet lifecycle states mirrored from the Fleet package so the API can
// coordinate operations without importing the Docker-backed Fleet service.
const (
	localFleetStateLaunching       = "launching"
	localFleetStateStarting        = "starting"
	localFleetStateRunning         = "running"
	localFleetStateStopping        = "stopping"
	localFleetStateStopped         = "stopped"
	localFleetStateRebooting       = "rebooting"
	localFleetStateTerminating     = "terminating"
	localFleetStateTerminated      = "terminated"
	localFleetStateFailed          = "failed"
	localFleetStateInstanceMissing = "instance_missing"

	localFleetOpPrepared = "prepared"
	localFleetOpAborted  = "aborted"
	localFleetOpFailed   = "failed"

	localFleetHealthFreshWindow = 30 * time.Second
	localFleetDiagnoseTimeout   = 5 * time.Second
	localFleetDiagnosePath      = "/api/v1/nodes/diagnose"
)

// Errors returned by Prepare. The HTTP proxy maps each into a stable status:
// 404 for a missing/foreign node, 409 for busy or conflicting requests.
var (
	ErrNodeBusy          = errors.New("node busy")
	ErrOperationConflict = errors.New("operation conflict")
	ErrNodeNotFound      = errors.New("node not found")
)

var (
	errLocalFleetRefRequired         = errors.New("exactly one of instance_id or node_id is required")
	errLocalFleetInvalidOperation    = errors.New("invalid local fleet operation")
	errLocalFleetIdempotencyMissing  = errors.New("idempotency key is required")
	errLocalFleetDiagnoseUnavailable = errors.New("node diagnosis unavailable")
)

// localFleetNodeRef addresses one node by Docker container id or node UUID.
// Exactly one of the two fields may be set.
type localFleetNodeRef struct {
	InstanceID string
	NodeID     string
}

// localFleetCoordinator serializes maintenance requests for local Fleet nodes
// against the shared PostgreSQL database. It only prepares and approves
// operations; the Fleet coordinator (a separate process) executes approved
// rows with the Docker provider.
type localFleetCoordinator struct {
	pool      *pgxpool.Pool
	namespace string
	// diagnose reads the node daemon's pending terminal reports through the
	// Fleet diagnostic endpoint. It runs outside every database transaction so
	// a slow Docker exec never holds a node lock.
	diagnose func(ctx context.Context, ownerID, containerID string) (int, error)
	now      func() time.Time
}

func newLocalFleetCoordinator(pool *pgxpool.Pool, namespace string, diagnose func(context.Context, string, string) (int, error), now func() time.Time) *localFleetCoordinator {
	if now == nil {
		now = time.Now
	}
	return &localFleetCoordinator{
		pool:      pool,
		namespace: strings.TrimSpace(namespace),
		diagnose:  diagnose,
		now:       now,
	}
}

func localFleetTargetState(operation string) (string, bool) {
	switch operation {
	case "stop":
		return localFleetStateStopping, true
	case "reboot":
		return localFleetStateRebooting, true
	case "delete":
		return localFleetStateTerminating, true
	default:
		return "", false
	}
}

// Prepare schedules one stop/reboot/delete operation.
//
// It runs in two short transactions separated by the Fleet diagnosis:
//
//  1. under the node's exclusive advisory lock, resolve the node, replay an
//     identical idempotency key, reject busy nodes, and persist the prepared
//     operation together with the transitional target state;
//  2. outside any transaction, read the daemon's pending terminal reports;
//  3. under the same lock, verify the prepared operation and generation, then
//     either abort the barrier (busy) or atomically approve the operation.
//
// The lock is the same barrier.Key namespace Fleet claims use, so a prepared
// operation blocks new work before the diagnosis starts.
func (c *localFleetCoordinator) Prepare(ctx context.Context, ownerID string, ref localFleetNodeRef, operation, idempotencyKey string) (db.FleetNode, error) {
	if c == nil || c.pool == nil {
		return db.FleetNode{}, errors.New("local fleet coordinator is not configured")
	}
	targetState, ok := localFleetTargetState(operation)
	if !ok {
		return db.FleetNode{}, fmt.Errorf("%w: %q", errLocalFleetInvalidOperation, operation)
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return db.FleetNode{}, errLocalFleetIdempotencyMissing
	}
	ownerUUID, err := util.ParseUUID(ownerID)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("owner id: %w", err)
	}

	node, op, replayed, err := c.prepare(ctx, ownerUUID, ref, operation, idempotencyKey, targetState)
	if err != nil {
		return db.FleetNode{}, err
	}
	if replayed {
		return node, nil
	}

	pending := 0
	if node.ContainerID != "" {
		if c.diagnose == nil {
			c.recordDiagnoseFailure(ctx, op)
			return db.FleetNode{}, errLocalFleetDiagnoseUnavailable
		}
		pending, err = c.diagnose(ctx, ownerID, node.ContainerID)
		if err != nil {
			c.recordDiagnoseFailure(ctx, op)
			return db.FleetNode{}, fmt.Errorf("%w: %v", errLocalFleetDiagnoseUnavailable, err)
		}
	}
	return c.finishPrepare(ctx, node, op, pending)
}

// prepare is the first short transaction. It returns replayed=true when the
// idempotency key already described this exact request, in which case no new
// operation or diagnosis is needed.
func (c *localFleetCoordinator) prepare(ctx context.Context, ownerID pgtype.UUID, ref localFleetNodeRef, operation, idempotencyKey, targetState string) (db.FleetNode, db.FleetNodeOperation, bool, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("begin prepare: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)

	node, err := c.loadNode(ctx, q, ownerID, ref)
	if err != nil {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, err
	}
	if err := barrier.AcquireExclusive(ctx, tx, barrier.Key(c.namespace, node.DaemonID)); err != nil {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, err
	}
	// Reload under the lock: the state checked below must be the state every
	// other coordinator sees while holding the same key.
	node, err = q.GetFleetNode(ctx, db.GetFleetNodeParams{ID: node.ID, Namespace: c.namespace})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FleetNode{}, db.FleetNodeOperation{}, false, ErrNodeNotFound
		}
		return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("reload node: %w", err)
	}

	fingerprint := localFleetRequestFingerprint(operation, node)
	existing, err := q.GetFleetNodeOperationByIdempotencyKey(ctx, idempotencyKey)
	switch {
	case err == nil:
		if existing.NodeID != node.ID || existing.RequestFingerprint != fingerprint {
			return db.FleetNode{}, db.FleetNodeOperation{}, false, ErrOperationConflict
		}
		switch existing.Phase {
		case localFleetOpPrepared:
			// A previous attempt prepared the barrier but never finished (for
			// example the diagnosis call failed). Re-run the outside-lock
			// diagnosis and approval for this same operation instead of
			// reporting an accepted operation that can never execute.
			if err := tx.Commit(ctx); err != nil {
				return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("commit prepared replay: %w", err)
			}
			return node, existing, false, nil
		case localFleetOpAborted:
			// The earlier attempt was rejected after diagnosis. Replaying it
			// must keep surfacing the same busy result instead of pretending a
			// maintenance operation is now approved.
			return db.FleetNode{}, db.FleetNodeOperation{}, false, ErrNodeBusy
		case localFleetOpFailed:
			return db.FleetNode{}, db.FleetNodeOperation{}, false, ErrOperationConflict
		default:
			if err := tx.Commit(ctx); err != nil {
				return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("commit idempotent prepare: %w", err)
			}
			return node, existing, true, nil
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("load idempotent operation: %w", err)
	}

	// Another maintenance operation still awaiting execution would race this
	// one for the same node. Refuse it instead of overwriting the prepared
	// generation that the first operation's approval check compares against.
	inflight, err := q.CountActiveFleetNodeOperationsForNode(ctx, node.ID)
	if err != nil {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("count active operations: %w", err)
	}
	if inflight > 0 {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, ErrOperationConflict
	}

	// R15: a launching node can still be creating its container and volumes,
	// so delete must wait until the launch settles.
	if operation == "delete" && node.TargetState == localFleetStateLaunching {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, ErrNodeBusy
	}

	// Busy checks cover every workspace whose runtime belongs to this owner;
	// the node itself is owner-scoped, not workspace-scoped.
	active, err := q.CountActiveTasksForFleetDaemon(ctx, db.CountActiveTasksForFleetDaemonParams{
		DaemonID: util.StrToText(node.DaemonID),
		OwnerID:  node.OwnerID,
	})
	if err != nil {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("count active tasks: %w", err)
	}
	if active > 0 {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, ErrNodeBusy
	}
	if operation == "delete" {
		queued, err := q.CountQueuedTasksForFleetDaemon(ctx, db.CountQueuedTasksForFleetDaemonParams{
			DaemonID: util.StrToText(node.DaemonID),
			OwnerID:  node.OwnerID,
		})
		if err != nil {
			return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("count queued tasks: %w", err)
		}
		if queued > 0 {
			return db.FleetNode{}, db.FleetNodeOperation{}, false, ErrNodeBusy
		}
	}

	if err := q.UpdateFleetNodeTargetState(ctx, db.UpdateFleetNodeTargetStateParams{
		ID:          node.ID,
		Namespace:   c.namespace,
		TargetState: targetState,
	}); err != nil {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("record maintenance target: %w", err)
	}
	node, err = q.GetFleetNode(ctx, db.GetFleetNodeParams{ID: node.ID, Namespace: c.namespace})
	if err != nil {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("reload prepared node: %w", err)
	}

	op, err := q.CreateFleetNodeOperation(ctx, db.CreateFleetNodeOperationParams{
		NodeID:             node.ID,
		Operation:          operation,
		Phase:              localFleetOpPrepared,
		IdempotencyKey:     idempotencyKey,
		RequestFingerprint: fingerprint,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return db.FleetNode{}, db.FleetNodeOperation{}, false, ErrOperationConflict
		}
		return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("create operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.FleetNode{}, db.FleetNodeOperation{}, false, fmt.Errorf("commit prepare: %w", err)
	}
	return node, op, false, nil
}

// finishPrepare is the second short transaction. It rechecks the generation,
// operation phase, and terminal state before approving; a pending report aborts
// the barrier and restores the prior running target.
func (c *localFleetCoordinator) finishPrepare(ctx context.Context, prepared db.FleetNode, op db.FleetNodeOperation, pending int) (db.FleetNode, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("begin approve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)

	if err := barrier.AcquireExclusive(ctx, tx, barrier.Key(c.namespace, prepared.DaemonID)); err != nil {
		return db.FleetNode{}, err
	}
	node, err := q.GetFleetNode(ctx, db.GetFleetNodeParams{ID: prepared.ID, Namespace: c.namespace})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FleetNode{}, ErrNodeNotFound
		}
		return db.FleetNode{}, fmt.Errorf("reload node for approval: %w", err)
	}
	if node.Generation != prepared.Generation {
		return db.FleetNode{}, ErrOperationConflict
	}
	if node.TargetState == localFleetStateTerminated || node.TargetState == localFleetStateFailed {
		return db.FleetNode{}, ErrOperationConflict
	}
	current, err := q.GetFleetNodeOperation(ctx, op.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FleetNode{}, ErrOperationConflict
		}
		return db.FleetNode{}, fmt.Errorf("reload operation: %w", err)
	}
	if current.Phase != localFleetOpPrepared {
		return db.FleetNode{}, ErrOperationConflict
	}

	if pending > 0 {
		affected, err := q.MarkFleetNodeOperationPhase(ctx, db.MarkFleetNodeOperationPhaseParams{
			ID:            op.ID,
			Phase:         localFleetOpAborted,
			ExpectedPhase: localFleetOpPrepared,
		})
		if err != nil {
			return db.FleetNode{}, fmt.Errorf("abort operation: %w", err)
		}
		if affected != 1 {
			return db.FleetNode{}, ErrOperationConflict
		}
		if err := q.UpdateFleetNodeTargetState(ctx, db.UpdateFleetNodeTargetStateParams{
			ID:          node.ID,
			Namespace:   c.namespace,
			TargetState: localFleetStateRunning,
		}); err != nil {
			return db.FleetNode{}, fmt.Errorf("restore node target state: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return db.FleetNode{}, fmt.Errorf("commit abort: %w", err)
		}
		return db.FleetNode{}, ErrNodeBusy
	}

	affected, err := q.ApproveFleetNodeOperation(ctx, op.ID)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("approve operation: %w", err)
	}
	if affected != 1 {
		return db.FleetNode{}, ErrOperationConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return db.FleetNode{}, fmt.Errorf("commit approve: %w", err)
	}
	return node, nil
}

func (c *localFleetCoordinator) loadNode(ctx context.Context, q *db.Queries, ownerID pgtype.UUID, ref localFleetNodeRef) (db.FleetNode, error) {
	instanceID := strings.TrimSpace(ref.InstanceID)
	nodeID := strings.TrimSpace(ref.NodeID)
	if (instanceID == "") == (nodeID == "") {
		return db.FleetNode{}, errLocalFleetRefRequired
	}
	if nodeID != "" {
		id, err := util.ParseUUID(nodeID)
		if err != nil {
			return db.FleetNode{}, ErrNodeNotFound
		}
		node, err := q.GetFleetNodeByIDAndOwner(ctx, db.GetFleetNodeByIDAndOwnerParams{
			ID:        id,
			Namespace: c.namespace,
			OwnerID:   ownerID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return db.FleetNode{}, ErrNodeNotFound
			}
			return db.FleetNode{}, fmt.Errorf("load node: %w", err)
		}
		return node, nil
	}

	node, err := q.GetFleetNodeByContainerID(ctx, db.GetFleetNodeByContainerIDParams{
		ContainerID: instanceID,
		Namespace:   c.namespace,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FleetNode{}, ErrNodeNotFound
		}
		return db.FleetNode{}, fmt.Errorf("load node: %w", err)
	}
	if node.OwnerID != ownerID {
		return db.FleetNode{}, ErrNodeNotFound
	}
	return node, nil
}

func (c *localFleetCoordinator) recordDiagnoseFailure(ctx context.Context, op db.FleetNodeOperation) {
	// Only attach the message while the row is still prepared: a concurrent
	// retry may already have approved or aborted it, and writing the phase back
	// would expand the maintenance barrier by accident.
	if _, err := db.New(c.pool).RecordFleetNodeOperationPrepareFailure(ctx, db.RecordFleetNodeOperationPrepareFailureParams{
		ID:    op.ID,
		Error: "diagnosis unavailable",
	}); err != nil {
		slog.Warn("local fleet: record diagnosis failure", "operation", uuidToString(op.ID), "error", err)
	}
}

func localFleetRequestFingerprint(operation string, node db.FleetNode) string {
	subject := node.ContainerID
	if subject == "" {
		subject = uuidToString(node.ID)
	}
	return operation + ":" + subject
}

// localFleetProxyOperation reports whether a proxied request is one this
// server coordinates locally. start/status/diagnose continue to pass through.
func localFleetProxyOperation(method, path string) (string, bool) {
	switch {
	case path == "/api/v1/nodes/stop" && method == http.MethodPost:
		return "stop", true
	case path == "/api/v1/nodes/reboot" && method == http.MethodPost:
		return "reboot", true
	case path == "/api/v1/nodes" && method == http.MethodDelete:
		return "delete", true
	default:
		return "", false
	}
}

func (h *Handler) coordinateLocalFleet(w http.ResponseWriter, r *http.Request, operation string) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}
	body, ok := readCloudRuntimeJSONBody(w, r)
	if !ok {
		return
	}
	var request struct {
		InstanceID string `json:"instance_id"`
		NodeID     string `json:"node_id"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	node, err := h.LocalFleet.Prepare(r.Context(), userID, localFleetNodeRef{
		InstanceID: request.InstanceID,
		NodeID:     request.NodeID,
	}, operation, idempotencyKey)
	if err != nil {
		writeLocalFleetError(w, r, err)
		return
	}
	now := time.Now
	if h.LocalFleet.now != nil {
		now = h.LocalFleet.now
	}
	writeJSON(w, http.StatusOK, localFleetNodeJSON(node, now()))
}

func writeLocalFleetError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNodeNotFound):
		writeError(w, http.StatusNotFound, "node not found")
	case errors.Is(err, ErrNodeBusy):
		writeError(w, http.StatusConflict, "node busy")
	case errors.Is(err, ErrOperationConflict):
		writeError(w, http.StatusConflict, "operation conflict")
	case errors.Is(err, errLocalFleetRefRequired),
		errors.Is(err, errLocalFleetInvalidOperation),
		errors.Is(err, errLocalFleetIdempotencyMissing):
		writeError(w, http.StatusBadRequest, "invalid request")
	default:
		slog.Warn("local fleet operation failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusServiceUnavailable, "local fleet operation failed")
	}
}

// localFleetDiagnose asks Fleet to read the node daemon's health and pending
// terminal reports. Fleet requires the owner identity, so it travels as the
// service-authenticated proxy's X-User-ID.
func (h *Handler) localFleetDiagnose(ctx context.Context, ownerID, containerID string) (int, error) {
	if h.CloudRuntime == nil {
		return 0, errors.New("cloud runtime client is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, localFleetDiagnoseTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"instance_id": containerID})
	if err != nil {
		return 0, err
	}
	resp, err := h.CloudRuntime.Do(ctx, cloudruntime.Request{
		Method: http.MethodPost,
		Path:   localFleetDiagnosePath,
		Body:   body,
		UserID: ownerID,
		Op:     "diagnose",
	})
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fleet diagnose returned status %d", resp.StatusCode)
	}
	var report struct {
		PendingTerminalReportCount int `json:"pending_terminal_report_count"`
	}
	if err := json.Unmarshal(resp.Body, &report); err != nil {
		return 0, fmt.Errorf("decode fleet diagnose: %w", err)
	}
	if report.PendingTerminalReportCount < 0 {
		return 0, errors.New("invalid pending terminal report count")
	}
	return report.PendingTerminalReportCount, nil
}

// localFleetNodeResponse mirrors Fleet's stable client node contract so local
// mode returns the same JSON shape as the proxied SaaS response.
type localFleetNodeResponse struct {
	ID           string            `json:"id"`
	OwnerID      string            `json:"owner_id"`
	InstanceID   string            `json:"instance_id"`
	Region       string            `json:"region"`
	InstanceType string            `json:"instance_type"`
	ImageID      string            `json:"image_id"`
	SubnetID     string            `json:"subnet_id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	Tags         map[string]string `json:"tags"`
	Metadata     map[string]any    `json:"metadata"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Provider     string            `json:"provider,omitempty"`
	Error        string            `json:"error,omitempty"`
}

func localFleetNodeJSON(node db.FleetNode, now time.Time) localFleetNodeResponse {
	return localFleetNodeResponse{
		ID:           uuidToString(node.ID),
		OwnerID:      uuidToString(node.OwnerID),
		InstanceID:   node.ContainerID,
		Region:       "local",
		InstanceType: localFleetNodeSpecID(node.Spec),
		ImageID:      node.Image,
		SubnetID:     "",
		Name:         node.Name,
		Status:       localFleetDisplayStatus(node, now),
		Tags:         map[string]string{},
		Metadata: map[string]any{
			"provider":       "docker",
			"daemon_id":      node.DaemonID,
			"target_state":   node.TargetState,
			"observed_state": node.ObservedState,
			"daemon_ready":   node.DaemonReady,
			"generation":     node.Generation,
		},
		CreatedAt: localFleetTimestamp(node.CreatedAt),
		UpdatedAt: localFleetTimestamp(node.UpdatedAt),
		Provider:  "docker",
		Error:     node.Error,
	}
}

func localFleetDisplayStatus(node db.FleetNode, now time.Time) string {
	containerExists := node.ContainerID != "" && node.ObservedState != localFleetStateInstanceMissing
	containerState := ""
	switch node.ObservedState {
	case localFleetStateRunning:
		containerState = "running"
	case localFleetStateStopped, localFleetStateTerminated:
		containerState = "exited"
	}
	healthy := node.ObservedState == localFleetStateRunning
	healthFresh := node.HealthCheckedAt.Valid && now.Sub(node.HealthCheckedAt.Time) < localFleetHealthFreshWindow

	switch node.TargetState {
	case localFleetStateLaunching:
		return localFleetStateLaunching
	case localFleetStateStarting:
		if !containerExists {
			return localFleetStateInstanceMissing
		}
		if containerState == "running" && healthy && node.DaemonReady && healthFresh {
			return localFleetStateRunning
		}
		return localFleetStateStarting
	case localFleetStateRunning:
		if !containerExists {
			return localFleetStateInstanceMissing
		}
		if containerState == "exited" {
			return localFleetStateStopped
		}
		if containerState == "running" && healthy && node.DaemonReady && healthFresh {
			return localFleetStateRunning
		}
		return localFleetStateStarting
	case localFleetStateStopping:
		return localFleetStateStopping
	case localFleetStateStopped:
		return localFleetStateStopped
	case localFleetStateRebooting:
		return localFleetStateRebooting
	case localFleetStateTerminating:
		return localFleetStateTerminating
	case localFleetStateTerminated:
		return localFleetStateTerminated
	default:
		return localFleetStateFailed
	}
}

func localFleetNodeSpecID(raw []byte) string {
	var spec struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return ""
	}
	return spec.ID
}

func localFleetTimestamp(value pgtype.Timestamptz) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}
