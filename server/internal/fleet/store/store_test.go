package store

import (
	"context"
	"errors"
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

// Readiness must not succeed on connectivity alone: a database whose
// migrations never ran cannot serve any Fleet request.
func TestPingRequiresFleetSchema(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping on a migrated database: %v", err)
	}
	if _, err := pool.Exec(ctx, "DROP TABLE fleet_node_operation"); err != nil {
		t.Fatalf("drop fleet_node_operation: %v", err)
	}
	if err := s.Ping(ctx); !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("Ping after dropping a required table = %v, want ErrSchemaMissing", err)
	}
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

func TestReserveNodeEnforcesPerOwnerLimitUnderConcurrency(t *testing.T) {
	pool := testPool(t)
	owner := pgtype.UUID{Bytes: [16]byte{9}, Valid: true}
	s := New(pool)
	makeParams := func(name string) db.CreateFleetNodeParams {
		return db.CreateFleetNodeParams{
			OwnerID: owner, Namespace: "wt-limit", Name: name, DaemonID: "daemon-" + name,
			Image: "img", Spec: []byte("{}"), TargetState: "launching",
		}
	}

	errs := make(chan error, 2)
	for _, name := range []string{"a", "b"} {
		go func(name string) {
			_, err := s.ReserveNode(context.Background(), makeParams(name), 1)
			errs <- err
		}(name)
	}
	var ok, limited int
	for i := 0; i < 2; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrLimitReached):
			limited++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || limited != 1 {
		t.Fatalf("ok=%d limited=%d, want 1/1", ok, limited)
	}
}

func TestStoreQueriesAreNamespaceScoped(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	owner := pgtype.UUID{Bytes: [16]byte{10}, Valid: true}
	node, err := s.Queries().CreateFleetNode(context.Background(), db.CreateFleetNodeParams{
		OwnerID: owner, Namespace: "wt-a", Name: "n", DaemonID: "daemon-a",
		Image: "img", Spec: []byte("{}"), TargetState: "launching",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{
		ID: node.ID, Namespace: "wt-b",
	}); err == nil {
		t.Fatal("cross-namespace read must miss")
	}
}

func TestRevokeCredentialsMakesHashUnresolvable(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	owner := pgtype.UUID{Bytes: [16]byte{11}, Valid: true}
	node, err := s.Queries().CreateFleetNode(context.Background(), db.CreateFleetNodeParams{
		OwnerID: owner, Namespace: "wt-c", Name: "n", DaemonID: "daemon-c",
		Image: "img", Spec: []byte("{}"), TargetState: "running",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Queries().CreateFleetNodeCredential(context.Background(), db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: "h-revoke",
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if err := s.RevokeCredentials(context.Background(), node.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err = s.Queries().GetFleetNodeCredentialByHash(context.Background(), "h-revoke")
	if err == nil {
		t.Fatal("revoked credential must not resolve")
	}
}

func TestUUIDText(t *testing.T) {
	id := pgtype.UUID{
		Bytes: [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef},
		Valid: true,
	}
	if got, want := UUIDText(id), "01234567-89ab-cdef-0123-456789abcdef"; got != want {
		t.Fatalf("UUIDText() = %q, want %q", got, want)
	}
	if got := UUIDText(pgtype.UUID{}); got != "" {
		t.Fatalf("UUIDText(invalid) = %q, want empty", got)
	}
}
