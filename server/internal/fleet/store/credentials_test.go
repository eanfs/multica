package store

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestNodeTokenEntropyAndHash(t *testing.T) {
	token, hash, e := NewNodeToken()
	if e != nil || !strings.HasPrefix(token, "mcn_") || hash != auth.HashToken(token) {
		t.Fatal("invalid token/hash")
	}
	raw, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "mcn_"))
	if e != nil || len(raw) != 32 {
		t.Fatal("token must have 32 random bytes")
	}
	second, _, e := NewNodeToken()
	if e != nil || token == second {
		t.Fatal("token reused")
	}
}

// Mint must rotate atomically without advancing the control-operation CAS generation.
func TestNodeTokenLifecycle(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-token"
	cleanProduced(t, f, ns)
	owner := ownerUUID(t, f.UserID)
	id := ownerUUID(t, f.FleetNode(t, ns, testutil.Cols{"generation": 42, "maintenance": true, "desired": "terminating"}))
	s := New(pool, ns)
	ctx := context.Background()
	first, g1, e := s.MintNodeToken(ctx, id)
	if e != nil || g1 != 1 {
		t.Fatalf("mint generation=%d err=%v", g1, e)
	}
	n, e := s.VerifyNodeToken(ctx, first)
	if e != nil || n.ID != id || n.Generation != 42 {
		t.Fatalf("verify/control=%+v err=%v", n, e)
	}
	second, g2, e := s.MintNodeToken(ctx, id)
	if e != nil || g2 != 2 || second == first {
		t.Fatalf("rotate generation=%d err=%v", g2, e)
	}
	if _, e = s.VerifyNodeToken(ctx, first); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("predecessor=%v", e)
	}
	if _, e = New(pool, "other").VerifyNodeToken(ctx, second); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("namespace=%v", e)
	}
	if e = s.UpsertProfiles(ctx, map[pgtype.UUID]string{owner: ""}, 1); e != nil {
		t.Fatal(e)
	}
	if _, e = s.VerifyNodeToken(ctx, second); e != nil {
		t.Fatalf("profile must not revoke=%v", e)
	}
	var rows string
	if e = pool.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(c))::text FROM fleet_node_credentials c WHERE namespace=$1 AND node_id=$2", ns, id).Scan(&rows); e != nil || strings.Contains(rows, first) || strings.Contains(rows, second) || !strings.Contains(rows, auth.HashToken(second)) {
		t.Fatal("DB credential secret/hash boundary")
	}
	for i := 0; i < 2; i++ {
		if e = s.RevokeNodeToken(ctx, id); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.VerifyNodeToken(ctx, second); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("immediate revoke=%v", e)
	}
	third, g3, e := s.MintNodeToken(ctx, id)
	if e != nil || g3 != 3 {
		t.Fatalf("history generation=%d err=%v", g3, e)
	}
	f.Exec(t, "UPDATE fleet_nodes SET revoked=true WHERE id=$1", id)
	if _, e = s.VerifyNodeToken(ctx, third); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("revoked node=%v", e)
	}
	if _, _, e = s.MintNodeToken(ctx, id); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("mint revoked=%v", e)
	}
	f.Exec(t, "UPDATE fleet_nodes SET revoked=false, status='terminated' WHERE id=$1", id)
	if _, _, e = s.MintNodeToken(ctx, id); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("mint terminal=%v", e)
	}
	f.Exec(t, "UPDATE fleet_nodes SET status='running', desired='running' WHERE id=$1", id)
	f.Exec(t, "UPDATE fleet_node_credentials SET generation=$1 WHERE node_id=$2 AND generation=3", int64(math.MaxInt64), id)
	if _, _, e = s.MintNodeToken(ctx, id); !errors.Is(e, model.ErrUnavailable) {
		t.Fatalf("overflow=%v", e)
	}
	if _, _, e = New(pool, "other").MintNodeToken(ctx, id); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("cross namespace mint=%v", e)
	}
}

// Removing owner/existence/hash/generation join checks must not authenticate an inconsistent row.
func TestNodeTokenPersistedIdentityChecks(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-token-integrity"
	cleanProduced(t, f, ns)
	s := New(pool, ns)
	ctx := context.Background()
	id := ownerUUID(t, f.FleetNode(t, ns))
	owner := ownerUUID(t, f.UserID)
	token, gen, e := s.MintNodeToken(ctx, id)
	if e != nil || gen != 1 {
		t.Fatal(e)
	}
	node, e := s.VerifyNodeToken(ctx, token)
	if e != nil || node.Resources != (model.Spec{CPUs: 2, MemoryBytes: 4294967296, Pids: 256, MaxRuns: 1}) {
		t.Fatalf("default snapshot=%+v err=%v", node.Resources, e)
	}
	for _, bad := range []string{"", token + "x", "mcn_" + strings.Repeat("!", 43), strings.Replace(token, "mcn_", "mat_", 1)} {
		if _, e = s.VerifyNodeToken(ctx, bad); !errors.Is(e, model.ErrForbidden) {
			t.Fatalf("malformed token=%v", e)
		}
	}
	other := ownerUUID(t, f.User(t, "other credential owner", "cred-other-"+f.UserID+"@test.invalid"))
	f.Exec(t, "UPDATE fleet_node_credentials SET owner_id=$1 WHERE namespace=$2 AND node_id=$3", other, ns, id)
	f.Cleanup(t, "DELETE FROM fleet_node_credentials WHERE namespace=$1 AND node_id=$2", ns, id)
	if _, e = s.VerifyNodeToken(ctx, token); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("mismatched credential owner=%v", e)
	}
	f.Exec(t, "UPDATE fleet_node_credentials SET owner_id=$1 WHERE namespace=$2 AND node_id=$3", owner, ns, id)
	f.Insert(t, "fleet_node_credentials", testutil.Cols{"namespace": ns, "owner_id": owner, "node_id": id, "token_hash": "not-a-token-hash", "generation": int64(2), "revoked_at": testutil.Raw("now()")})
	if _, e = s.VerifyNodeToken(ctx, token); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("superseded generation=%v", e)
	}
	orphan := ownerUUID(t, "11111111-1111-4111-8111-111111111111")
	f.Exec(t, "UPDATE fleet_nodes SET owner_id=$1 WHERE id=$2", orphan, id)
	f.Exec(t, "UPDATE fleet_node_credentials SET owner_id=$1 WHERE namespace=$2 AND node_id=$3", orphan, ns, id)
	if _, e = s.VerifyNodeToken(ctx, token); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("missing user=%v", e)
	}
	if _, _, e = s.MintNodeToken(ctx, id); !errors.Is(e, model.ErrForbidden) {
		t.Fatalf("mint missing user=%v", e)
	}
}

func TestNodeTokenConcurrentMintRevoke(t *testing.T) {
	pool, f := testutil.NewFleetFixture(t)
	ns := "test-mint-race"
	cleanProduced(t, f, ns)
	id := ownerUUID(t, f.FleetNode(t, ns))
	s := New(pool, ns)
	start := make(chan struct{})
	var wg sync.WaitGroup
	type result struct {
		token string
		gen   int64
		err   error
	}
	out := make(chan result, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			token, g, e := s.MintNodeToken(context.Background(), id)
			out <- result{token, g, e}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	var latest string
	gens := map[int64]bool{}
	for r := range out {
		if r.err != nil {
			t.Fatal(r.err)
		}
		gens[r.gen] = true
		if r.gen == 2 {
			latest = r.token
		} else if _, e := s.VerifyNodeToken(context.Background(), r.token); !errors.Is(e, model.ErrForbidden) {
			t.Fatalf("old concurrent token=%v", e)
		}
	}
	if !gens[1] || !gens[2] {
		t.Fatalf("generations=%v", gens)
	}
	if _, e := s.VerifyNodeToken(context.Background(), latest); e != nil {
		t.Fatal(e)
	}
	start = make(chan struct{})
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, _, e := s.MintNodeToken(context.Background(), id); errs <- e }()
	go func() { defer wg.Done(); <-start; errs <- s.RevokeNodeToken(context.Background(), id) }()
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var active, history int
	if e := pool.QueryRow(context.Background(), "SELECT count(*) FILTER (WHERE revoked_at IS NULL),count(*) FROM fleet_node_credentials WHERE namespace=$1 AND node_id=$2", ns, id).Scan(&active, &history); e != nil || active > 1 || history != 3 {
		t.Fatalf("active=%d history=%d err=%v", active, history, e)
	}
	if e := s.RevokeNodeToken(context.Background(), id); e != nil {
		t.Fatal(e)
	}
}
