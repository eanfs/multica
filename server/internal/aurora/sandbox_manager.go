package aurora

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// WorkspaceSandboxManager provisions or confirms the workspace's managed
// sandbox before a generation reserves credits, and hands that sandbox to the
// Fleet lifecycle before workspace teardown deletes its row. The handler depends
// on this interface, so a test can watch and fail the fleet step independently
// of the concrete client.
type WorkspaceSandboxManager interface {
	Ensure(ctx context.Context, workspaceID, runtimeID pgtype.UUID) (db.AuroraSandboxNode, error)
	// HandoffWorkspaceToFleet enqueues the Fleet destroy intent for the
	// workspace's sandbox node so its container and volumes are removed. It must
	// run before the teardown transaction opens — the node row is read through
	// the pool and no database lock may be held across the Fleet HTTP call (see
	// DeleteWorkspace).
	HandoffWorkspaceToFleet(ctx context.Context, workspaceID pgtype.UUID) error
}

// SandboxManager is the production WorkspaceSandboxManager. It owns the
// workspace's single node row and the single-use enrollment that admits a
// managed daemon to it, and it drives the fleet control API to bring the node
// up.
type SandboxManager struct {
	queries     *db.Queries
	tx          TxBeginner
	fleet       FleetProvisioner
	imageDigest string
	now         func() time.Time
}

// sandboxArm is one arm decision: the scrubbed node, the Fleet node owner read
// under the same lock, the raw secret to ship (empty when adopting an existing
// node), and whether the Fleet must be called at all.
type sandboxArm struct {
	node      db.AuroraSandboxNode
	ownerID   string
	token     string
	needFleet bool
}

// NewSandboxManager wires the manager to its database, provisioner, deployed
// image digest, and clock. imageDigest must be the same pinned reference the
// fleet runs, so a node armed for a different image is re-provisioned. now
// defaults to time.Now only when omitted.
func NewSandboxManager(queries *db.Queries, tx TxBeginner, fleet FleetProvisioner, imageDigest string, now func() time.Time) *SandboxManager {
	if now == nil {
		now = time.Now
	}
	return &SandboxManager{queries: queries, tx: tx, fleet: fleet, imageDigest: imageDigest, now: now}
}

// newSandboxDaemonID mints the daemon identity bound to one sandbox node. The
// fleet's control API validates node, workspace, runtime, and daemon identity as
// canonical UUIDs, and the managed daemon adopts whatever identity the server
// assigns at enrollment, so this is a plain UUID with no prefix or decoration.
func newSandboxDaemonID() string {
	return uuid.NewString()
}

// Ensure makes the workspace's sandbox node ready to run its managed runtime.
//
// The decision and any enrollment write happen under the same per-workspace
// advisory lock enrollment issuance uses, so concurrent callers converge on one
// node and one secret. The fleet HTTP call runs strictly after that transaction
// commits: a database lock is never held across network I/O, and a fleet
// failure marks the node failed with its enrollment cleared rather than leaving
// a live secret behind.
func (m *SandboxManager) Ensure(ctx context.Context, workspaceID, runtimeID pgtype.UUID) (db.AuroraSandboxNode, error) {
	if !validImageDigest(m.imageDigest) {
		return db.AuroraSandboxNode{}, fmt.Errorf("sandbox image %q must end with @sha256: and 64 lowercase hex characters", m.imageDigest)
	}

	armed, err := m.arm(ctx, workspaceID, runtimeID)
	if err != nil {
		return db.AuroraSandboxNode{}, err
	}
	if !armed.needFleet {
		return armed.node, nil
	}

	fleetNode, err := m.fleet.EnsureWorkspaceNode(ctx, armed.ownerID, FleetEnsureRequest{
		NodeID:          util.UUIDToString(armed.node.ID),
		WorkspaceID:     util.UUIDToString(workspaceID),
		RuntimeID:       util.UUIDToString(runtimeID),
		DaemonID:        armed.node.DaemonID,
		EnrollmentToken: armed.token,
		ImageDigest:     m.imageDigest,
		Name:            auroraFleetNodeName,
		Spec:            auroraFleetNodeSpec,
	})
	if err != nil {
		m.markFailed(ctx, workspaceID, armed.node.ID, err)
		return db.AuroraSandboxNode{}, fmt.Errorf("ensure workspace sandbox: %w", err)
	}

	// Persist the backend id the fleet assigned. The node stays starting until
	// its daemon spends the enrollment secret, so this is bookkeeping only.
	updated, err := m.queries.SetAuroraSandboxNodeBackend(ctx, db.SetAuroraSandboxNodeBackendParams{
		ID:            armed.node.ID,
		BackendNodeID: pgtype.Text{String: fleetNode.ID, Valid: fleetNode.ID != ""},
		WorkspaceID:   workspaceID,
	})
	if err != nil {
		return db.AuroraSandboxNode{}, fmt.Errorf("record sandbox backend node: %w", err)
	}
	return scrubEnrollment(updated), nil
}

// HandoffWorkspaceToFleet enqueues the Fleet destroy intent for the workspace's
// managed sandbox node. Workspace teardown calls it before its transaction
// opens, so the node row is read through the pool while no database lock is
// held: the row is the reaper's only index into the node, and a row deleted
// first strands the node's container plus its data/secrets volumes with no later
// path that can see them.
//
// The intent is exactly the one the reaper records through the shared
// deleteFleetWorkspaceNode seam, so the Fleet reconciler removes the container
// and volumes and revokes the node credential. A workspace that never
// provisioned a sandbox, or whose node never reached the Fleet, is a no-op; a
// node that did reach the Fleet must be handed off, so a Fleet failure is
// returned to the caller rather than swallowed.
func (m *SandboxManager) HandoffWorkspaceToFleet(ctx context.Context, workspaceID pgtype.UUID) error {
	node, err := m.queries.GetAuroraSandboxNodeByWorkspace(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("load sandbox node for Fleet handoff: %w", err)
	}
	return deleteFleetWorkspaceNode(ctx, m.queries, m.fleet, node)
}

// arm decides whether the existing node already satisfies the request and, when
// it does not, arms a fresh enrollment. It returns the scrubbed node, the
// locked runtime owner the Fleet call must carry, the raw secret (empty when no
// new secret was minted), and whether the fleet must be called. The workspace
// advisory lock is held for the whole decision, so concurrent callers cannot
// both arm a node.
func (m *SandboxManager) arm(ctx context.Context, workspaceID, runtimeID pgtype.UUID) (sandboxArm, error) {
	tx, err := m.tx.Begin(ctx)
	if err != nil {
		return sandboxArm{}, err
	}
	defer tx.Rollback(ctx)
	qtx := m.queries.WithTx(tx)

	if err := qtx.LockAuroraSandboxEnrollmentWorkspace(ctx, workspaceID); err != nil {
		return sandboxArm{}, fmt.Errorf("lock workspace enrollment: %w", err)
	}
	managed, err := qtx.GetAuroraManagedRuntime(ctx, db.GetAuroraManagedRuntimeParams{
		WorkspaceID: workspaceID,
		Provider:    managedRuntimeProvider,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sandboxArm{}, fmt.Errorf("workspace %s has no managed runtime", util.UUIDToString(workspaceID))
		}
		return sandboxArm{}, err
	}
	if managed.ID != runtimeID {
		return sandboxArm{}, fmt.Errorf(
			"runtime %s is not the workspace's managed runtime %s",
			util.UUIDToString(runtimeID), util.UUIDToString(managed.ID))
	}
	ownerID := util.UUIDToString(managed.OwnerID)

	existing, err := qtx.LockAuroraSandboxNodeByWorkspace(ctx, workspaceID)
	noRow := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !noRow {
		return sandboxArm{}, err
	}
	if !noRow && m.reusable(existing, runtimeID) &&
		(existing.State == "online" || m.freshStarting(existing)) {
		// Already serving, or a live secret armed by another caller is still
		// pending: adopt the row instead of calling the fleet again.
		if err := tx.Commit(ctx); err != nil {
			return sandboxArm{}, err
		}
		return sandboxArm{node: scrubEnrollment(existing), ownerID: ownerID}, nil
	}

	raw, err := auth.GenerateManagedEnrollmentToken()
	if err != nil {
		return sandboxArm{}, err
	}
	expiresAt := m.now().Add(enrollmentTokenTTL)
	hash := pgtype.Text{String: auth.HashToken(raw), Valid: true}
	expiry := pgtype.Timestamptz{Time: expiresAt, Valid: true}

	var node db.AuroraSandboxNode
	if noRow {
		nodeID := util.MustParseUUID(uuid.NewString())
		node, err = qtx.CreateAuroraSandboxNode(ctx, db.CreateAuroraSandboxNodeParams{
			ID:                  nodeID,
			WorkspaceID:         workspaceID,
			RuntimeID:           runtimeID,
			DaemonID:            newSandboxDaemonID(),
			ImageDigest:         m.imageDigest,
			State:               "starting",
			EnrollmentTokenHash: hash,
			EnrollmentExpiresAt: expiry,
		})
		if err != nil {
			return sandboxArm{}, fmt.Errorf("create sandbox node: %w", err)
		}
	} else {
		// Stopped, failed, stale-starting, or derived from another runtime or
		// image: re-arm the same row with the current runtime, image, and one
		// fresh secret.
		node, err = qtx.ReArmAuroraSandboxNode(ctx, db.ReArmAuroraSandboxNodeParams{
			WorkspaceID:         workspaceID,
			RuntimeID:           runtimeID,
			ImageDigest:         m.imageDigest,
			EnrollmentTokenHash: hash,
			EnrollmentExpiresAt: expiry,
		})
		if err != nil {
			return sandboxArm{}, fmt.Errorf("re-arm sandbox node: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return sandboxArm{}, err
	}
	return sandboxArm{node: scrubEnrollment(node), ownerID: ownerID, token: raw, needFleet: true}, nil
}

// reusable reports whether the node is bound to the requested runtime and was
// armed for the deployed image.
func (m *SandboxManager) reusable(node db.AuroraSandboxNode, runtimeID pgtype.UUID) bool {
	return node.RuntimeID == runtimeID && node.ImageDigest == m.imageDigest
}

// freshStarting reports whether the node carries an unspent, unexpired
// enrollment secret. Such a node is mid-provision and must not be re-armed.
func (m *SandboxManager) freshStarting(node db.AuroraSandboxNode) bool {
	return node.State == "starting" &&
		node.EnrollmentTokenHash.Valid &&
		node.EnrollmentExpiresAt.Valid &&
		!node.EnrollmentConsumedAt.Valid &&
		node.EnrollmentExpiresAt.Time.After(m.now())
}

// markFailed records a failed fleet ensure. The write is best-effort: the
// request is already failing, but a node left starting with a live enrollment
// would be worse, so the miss is logged.
func (m *SandboxManager) markFailed(ctx context.Context, workspaceID, nodeID pgtype.UUID, cause error) {
	if _, err := m.queries.FailAuroraSandboxNode(ctx, db.FailAuroraSandboxNodeParams{
		ID:            nodeID,
		WorkspaceID:   workspaceID,
		FailureReason: pgtype.Text{String: cause.Error(), Valid: true},
	}); err != nil {
		slog.Warn("aurora sandbox fail-mark error",
			"workspace_id", util.UUIDToString(workspaceID),
			"node_id", util.UUIDToString(nodeID),
			"error", err)
	}
}

// scrubEnrollment removes the secret fields from a node before it is returned.
// The raw secret is delivered to the fleet, and its hash stays in the database;
// no caller of Ensure needs either.
func scrubEnrollment(node db.AuroraSandboxNode) db.AuroraSandboxNode {
	node.EnrollmentTokenHash = pgtype.Text{}
	node.EnrollmentExpiresAt = pgtype.Timestamptz{}
	return node
}
