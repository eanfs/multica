package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	guuid "github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
)

// TestFleetAuroraBootstrapConfirm is the Task 3 fix regression. The Aurora create
// path carries no credential profile, so it has no Fleet node token to mint; it
// must still mark the operation bootstrapped so the existing SQL/lease fence in
// ConfirmBootstrap keeps working. The test drives the real store end to end:
// CreateAuroraIntent -> ClaimBootstrap -> MarkBootstrapMinted -> ConfirmBootstrap.
//
// It asserts the fence stays authoritative: an unmarked create fails closed with
// ErrConflict and mutates no node, the mark writes no credential row, and the
// marked create confirms the observed container.
func TestFleetAuroraBootstrapConfirm(t *testing.T) {
	s, f, ns := auroraStoreFixture(t, 2)
	ctx := context.Background()
	owner := uuid(t, f.UserID)
	nodeID := uuid(t, guuid.NewString())
	req := model.AuroraNodeRequest{
		WorkspaceID:     uuid(t, f.WorkspaceID),
		RuntimeID:       uuid(t, guuid.NewString()),
		DaemonID:        guuid.NewString(),
		ImageDigest:     "aurora-test-image",
		Name:            "aurora-bootstrap",
		Spec:            "sandbox",
		IdempotencyKey:  "aurora-bootstrap-key",
		EnrollmentToken: "mse_" + strings.Repeat("a", 40),
	}
	node, op, replayed, err := s.CreateAuroraIntent(ctx, owner, nodeID, req)
	if err != nil || replayed {
		t.Fatalf("create node=%+v replayed=%v err=%v", node, replayed, err)
	}
	ref := model.OperationRef{Namespace: ns, NodeID: node.ID, OperationID: op.ID, Generation: op.Generation, Action: model.Create}
	claim, err := s.ClaimBootstrap(ctx, owner, ref)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	obs := model.Observation{ContainerID: "aurora-cid", Status: "running", DaemonID: node.DaemonID, StartEpoch: time.Now().UTC().Format(time.RFC3339Nano), ObservedAt: time.Now().UTC(), Ready: true, ReportStatsKnown: true}

	// An unmarked create must fail closed: the fence stays authoritative and the
	// node is neither adopted nor revoked.
	if err = s.ConfirmBootstrap(ctx, claim, obs); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("unmarked Confirm = %v, want %v", err, model.ErrConflict)
	}
	if got, err := s.GetAuroraNode(ctx, owner, nodeID); err != nil || got.ContainerID != "" || got.Revoked {
		t.Fatalf("unmarked node mutated: %+v err=%v", got, err)
	}

	// The Aurora branch marks the operation bootstrapped without a node token.
	if err = s.MarkBootstrapMinted(ctx, claim); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if n := f.Count(t, "SELECT count(*) FROM fleet_node_credentials WHERE namespace=$1 AND owner_id=$2 AND node_id=$3", ns, f.UserID, util.UUIDToString(nodeID)); n != 0 {
		t.Fatalf("mark wrote %d credential rows", n)
	}
	if err = s.MarkBootstrapMinted(ctx, claim); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("second mark = %v, want %v", err, model.ErrConflict)
	}

	// The mark satisfies the minted precondition; the fence still checks the
	// container/claim and the observed result is persisted.
	if err = s.ConfirmBootstrap(ctx, claim, obs); err != nil {
		t.Fatalf("marked Confirm: %v", err)
	}
	got, err := s.GetAuroraNode(ctx, owner, nodeID)
	if err != nil || got.ContainerID != obs.ContainerID || !got.Ready || got.Revoked {
		t.Fatalf("confirmed node = %+v err=%v", got, err)
	}
	current, err := s.GetOperation(ctx, owner, op.ID)
	if err != nil || !current.BootstrapMinted {
		t.Fatalf("confirmed operation = %+v err=%v", current, err)
	}
}
