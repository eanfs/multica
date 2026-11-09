package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	return testutil.IsolatedDB(t, dsn)
}

func TestCreateFleetNodePersistsNamespaceAndSpec(t *testing.T) {
	pool := testPool(t)
	q := db.New(pool)
	owner := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	node, err := q.CreateFleetNode(context.Background(), db.CreateFleetNodeParams{
		OwnerID:       owner,
		Namespace:     "wt-test",
		Name:          "node-1",
		DaemonID:      "daemon-node-1",
		Image:         "multica/runtime-claude:test",
		Spec:          []byte("{\"cpu\":2,\"memory_mb\":4096}"),
		TargetState:   "launching",
		ObservedState: "",
	})
	if err != nil {
		t.Fatalf("create fleet node: %v", err)
	}
	if node.Namespace != "wt-test" || node.DaemonID != "daemon-node-1" {
		t.Fatalf("unexpected node: %+v", node)
	}
	got, err := q.GetFleetNodeByDaemonID(context.Background(), db.GetFleetNodeByDaemonIDParams{
		DaemonID: "daemon-node-1", Namespace: "wt-test",
	})
	if err != nil {
		t.Fatalf("get by daemon: %v", err)
	}
	if got.ID != node.ID {
		t.Fatalf("got node %s, want %s", got.ID, node.ID)
	}
}

func TestCreateFleetNodeCredentialRejectsDuplicateHash(t *testing.T) {
	pool := testPool(t)
	q := db.New(pool)
	owner := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	node, err := q.CreateFleetNode(context.Background(), db.CreateFleetNodeParams{
		OwnerID: owner, Namespace: "wt-test", Name: "node-2", DaemonID: "daemon-node-2",
		Image: "img", Spec: []byte("{}"), TargetState: "launching",
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := q.CreateFleetNodeCredential(context.Background(), db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: "hash-1",
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if _, err := q.CreateFleetNodeCredential(context.Background(), db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: "hash-1",
	}); err == nil {
		t.Fatal("duplicate token_hash must fail the unique index")
	}
}
