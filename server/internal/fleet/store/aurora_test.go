package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	guuid "github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// auroraStoreFixture builds an Aurora-profile store on a unique namespace, so
// quota and idempotency assertions cannot observe another test's rows.
func auroraStoreFixture(t *testing.T, maxNodes int) (*Store, *testutil.Fixture, string) {
	t.Helper()
	pool, f := testutil.NewFleetFixture(t)
	ns := "aurora-" + f.UserID
	f.Cleanup(t, "DELETE FROM fleet_node_operations WHERE namespace=$1", ns)
	f.Cleanup(t, "DELETE FROM fleet_nodes WHERE namespace=$1", ns)
	cfg := model.Config{
		Namespace: ns,
		Image:     "aurora-test-image",
		Specs:     map[string]model.Spec{"sandbox": {CPUs: 2, MemoryBytes: 4294967296, Pids: 256, MaxRuns: 1}},
		Aurora:    &model.AuroraConfig{ServerURL: "http://api.test"},
	}
	return New(pool, ns, WithProvisioningConfig(cfg), WithMaxNodes(maxNodes)), f, ns
}

// TestFleetAuroraIntent is the Task 3 producer regression: caller-supplied node
// and daemon identity is persisted as-is, a matching idempotency replay returns
// the original node/operation even when the one-time enrollment secret differs,
// a conflicting replay or reused node UUID is rejected, quota is enforced, and
// the enrollment secret never reaches SQL.
func TestFleetAuroraIntent(t *testing.T) {
	s, f, ns := auroraStoreFixture(t, 2)
	ctx := context.Background()
	owner := uuid(t, f.UserID)
	nodeID := uuid(t, guuid.NewString())
	req := model.AuroraNodeRequest{
		WorkspaceID:     uuid(t, f.WorkspaceID),
		RuntimeID:       uuid(t, guuid.NewString()),
		DaemonID:        guuid.NewString(),
		ImageDigest:     "aurora-test-image",
		Name:            "aurora-node",
		Spec:            "sandbox",
		IdempotencyKey:  "aurora-key-1",
		EnrollmentToken: "mse_" + strings.Repeat("a", 40),
	}

	node, op, replayed, err := s.CreateAuroraIntent(ctx, owner, nodeID, req)
	if err != nil || replayed {
		t.Fatalf("create node=%+v op=%+v replayed=%v err=%v", node, op, replayed, err)
	}
	if node.ID != nodeID || node.DaemonID != req.DaemonID || node.Image != req.ImageDigest || node.OwnerID != owner {
		t.Fatalf("created node identity = %+v", node)
	}
	if node.Resources != (model.Spec{CPUs: 2, MemoryBytes: 4294967296, Pids: 256, MaxRuns: 1}) {
		t.Fatalf("created resources = %+v", node.Resources)
	}
	if op.Action != model.Create || op.NodeID != nodeID || op.IdempotencyKey != req.IdempotencyKey {
		t.Fatalf("created operation = %+v", op)
	}

	// The caller-supplied UUIDs are the durable identity, not generated defaults.
	var workspaceID, runtimeID, daemonID string
	f.QueryRow(t, "SELECT workspace_id::text, runtime_id::text, daemon_id::text FROM fleet_nodes WHERE namespace=$1 AND owner_id=$2 AND id=$3", ns, f.UserID, util.UUIDToString(nodeID)).Scan(&workspaceID, &runtimeID, &daemonID)
	if workspaceID != util.UUIDToString(req.WorkspaceID) || runtimeID != util.UUIDToString(req.RuntimeID) || daemonID != req.DaemonID {
		t.Fatalf("persisted dimensions = %q %q %q", workspaceID, runtimeID, daemonID)
	}

	// Same key, same identity: replay returns the original node/op. A rotated
	// one-time secret is not part of the idempotency fingerprint.
	rotated := req
	rotated.EnrollmentToken = "mse_" + strings.Repeat("b", 40)
	node2, op2, replayed, err := s.CreateAuroraIntent(ctx, owner, nodeID, rotated)
	if err != nil || !replayed || node2.ID != node.ID || op2.ID != op.ID {
		t.Fatalf("replay node=%+v op=%+v replayed=%v err=%v", node2, op2, replayed, err)
	}

	// Different payload under the same key is a conflict, never a silent rebuild.
	different := req
	different.Name = "other-name"
	if _, _, _, err = s.CreateAuroraIntent(ctx, owner, nodeID, different); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("different payload err=%v", err)
	}

	// The same key resolving to a different node UUID is a conflict.
	if _, _, _, err = s.CreateAuroraIntent(ctx, owner, uuid(t, guuid.NewString()), req); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("rebound node err=%v", err)
	}

	// A second distinct node is admitted; a third exceeds the owner limit.
	second := req
	second.IdempotencyKey = "aurora-key-2"
	second.Name = "aurora-node-2"
	second.DaemonID = guuid.NewString()
	if _, _, _, err = s.CreateAuroraIntent(ctx, owner, uuid(t, guuid.NewString()), second); err != nil {
		t.Fatalf("second node err=%v", err)
	}
	third := req
	third.IdempotencyKey = "aurora-key-3"
	third.Name = "aurora-node-3"
	third.DaemonID = guuid.NewString()
	if _, _, _, err = s.CreateAuroraIntent(ctx, owner, uuid(t, guuid.NewString()), third); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("quota err=%v", err)
	}

	// The enrollment secret is never persisted, in any column of either table.
	secretMarker := strings.TrimPrefix(req.EnrollmentToken, "mse_")
	if n := f.Count(t, "SELECT count(*) FROM fleet_nodes WHERE namespace=$1 AND owner_id=$2 AND (name LIKE $3 OR spec LIKE $3 OR image LIKE $3 OR spec_config::text LIKE $3)", ns, f.UserID, "%mse_%"); n != 0 {
		t.Fatalf("secret leaked into fleet_nodes: %d", n)
	}
	if n := f.Count(t, "SELECT count(*) FROM fleet_node_operations WHERE namespace=$1 AND owner_id=$2 AND (idempotency_key LIKE $3 OR request_hash LIKE $3)", ns, f.UserID, "%"+secretMarker+"%"); n != 0 {
		t.Fatalf("secret leaked into fleet_node_operations: %d", n)
	}

	// Reads are owner- and namespace-scoped.
	got, err := s.GetAuroraNode(ctx, owner, nodeID)
	if err != nil || got.ID != nodeID || got.DaemonID != req.DaemonID {
		t.Fatalf("get aurora node = %+v err=%v", got, err)
	}
	other := f.User(t, "aurora-other", "aurora-other-"+f.UserID+"@test.invalid")
	if _, err = s.GetAuroraNode(ctx, uuid(t, other), nodeID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-owner err=%v", err)
	}
	if _, err = New(s.pool, "wrong-namespace").GetAuroraNode(ctx, owner, nodeID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-namespace err=%v", err)
	}
}
