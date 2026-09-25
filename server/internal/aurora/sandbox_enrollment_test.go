package aurora_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// newSandboxNodeWorkspace builds a throwaway workspace with the managed runtime
// Task 1's sandbox-node rows point at. Nodes carry no foreign key, so their
// rows are removed by workspace id before the workspace cascades away.
func newSandboxNodeWorkspace(t *testing.T, q *db.Queries, pool *pgxpool.Pool) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	owner := newAuroraTestUser(t, pool)
	ws := newAuroraTestWorkspace(t, pool, owner)
	if err := aurora.EnsureSystemAgents(ctx, q, ws, owner); err != nil {
		t.Fatalf("ensure system agents: %v", err)
	}
	runtimeID, err := aurora.ManagedRuntimeID(ctx, q, ws)
	if err != nil {
		t.Fatalf("managed runtime id: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM aurora_sandbox_node WHERE workspace_id = $1`, ws)
		_, _ = pool.Exec(ctx, `DELETE FROM agent_skill WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1)`, ws)
		_, _ = pool.Exec(ctx, `DELETE FROM agent WHERE workspace_id = $1`, ws)
		_, _ = pool.Exec(ctx, `DELETE FROM skill WHERE workspace_id = $1`, ws)
		_, _ = pool.Exec(ctx, `DELETE FROM agent_runtime WHERE workspace_id = $1`, ws)
	})
	return ws, runtimeID
}

// sandboxNodeParams is a valid starting node row. The enrollment hash is unique
// per call so an intended duplicate is the only constraint it can hit.
func sandboxNodeParams(t *testing.T, ws, runtimeID pgtype.UUID, daemonID string) db.CreateAuroraSandboxNodeParams {
	t.Helper()
	raw := "mse_" + uuid.NewString()
	return db.CreateAuroraSandboxNodeParams{
		ID:                  mustUUID(t, uuid.NewString()),
		WorkspaceID:         ws,
		RuntimeID:           runtimeID,
		DaemonID:            daemonID,
		ImageDigest:         "ghcr.io/eanfs/multica-aurora@sha256:" + strings.Repeat("a", 64),
		State:               "starting",
		EnrollmentTokenHash: pgtype.Text{String: auth.HashToken(raw), Valid: true},
		EnrollmentExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}
}

func requireUniqueViolation(t *testing.T, err error, indexName string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want unique violation on %s, got %v", indexName, err)
	}
	if pgErr.Code != "23505" {
		t.Fatalf("want unique violation on %s, got SQLSTATE %s: %v", indexName, pgErr.Code, err)
	}
	if pgErr.ConstraintName != indexName {
		t.Fatalf("unique violation on %s, want %s", pgErr.ConstraintName, indexName)
	}
}

// TestAuroraSandboxNodeUniquenessAndConsumption pins the Task 1 schema
// invariants: one node per workspace, runtime, and daemon identity, and an
// enrollment hash that is consumable exactly once and only before it expires.
func TestAuroraSandboxNodeUniquenessAndConsumption(t *testing.T) {
	pool := auroraTestPool(t)
	q := db.New(pool)
	ctx := context.Background()

	t.Run("identity uniqueness", func(t *testing.T) {
		wsA, rtA := newSandboxNodeWorkspace(t, q, pool)
		daemonA := "aurora-" + uuid.NewString()
		if _, err := q.CreateAuroraSandboxNode(ctx, sandboxNodeParams(t, wsA, rtA, daemonA)); err != nil {
			t.Fatalf("create first node: %v", err)
		}

		// A second row for the same workspace collides on workspace_id even
		// though its runtime and daemon identities are fresh.
		_, err := q.CreateAuroraSandboxNode(ctx, sandboxNodeParams(t, wsA, mustUUID(t, uuid.NewString()), "aurora-"+uuid.NewString()))
		requireUniqueViolation(t, err, "aurora_sandbox_node_workspace_uidx")

		// A second workspace may not reuse the first row's runtime.
		wsB, _ := newSandboxNodeWorkspace(t, q, pool)
		_, err = q.CreateAuroraSandboxNode(ctx, sandboxNodeParams(t, wsB, rtA, "aurora-"+uuid.NewString()))
		requireUniqueViolation(t, err, "aurora_sandbox_node_runtime_uidx")

		// A second workspace may not reuse the first row's daemon identity.
		wsC, rtC := newSandboxNodeWorkspace(t, q, pool)
		_, err = q.CreateAuroraSandboxNode(ctx, sandboxNodeParams(t, wsC, rtC, daemonA))
		requireUniqueViolation(t, err, "aurora_sandbox_node_daemon_uidx")
	})

	t.Run("consumable exactly once before expiry", func(t *testing.T) {
		ws, rt := newSandboxNodeWorkspace(t, q, pool)
		raw := "mse_" + uuid.NewString()
		hash := auth.HashToken(raw)
		params := sandboxNodeParams(t, ws, rt, "aurora-"+uuid.NewString())
		params.EnrollmentTokenHash = pgtype.Text{String: hash, Valid: true}
		params.EnrollmentExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}
		if _, err := q.CreateAuroraSandboxNode(ctx, params); err != nil {
			t.Fatalf("create starting node: %v", err)
		}

		consumed, err := q.ConsumeAuroraSandboxEnrollment(ctx, pgtype.Text{String: hash, Valid: true})
		if err != nil {
			t.Fatalf("consume enrollment: %v", err)
		}
		if consumed.State != "online" {
			t.Errorf("consumed state = %q, want online", consumed.State)
		}
		if consumed.EnrollmentTokenHash.Valid {
			t.Errorf("consumed hash = %q, want NULL", consumed.EnrollmentTokenHash.String)
		}
		if !consumed.EnrollmentConsumedAt.Valid {
			t.Error("consumed_at is NULL, want a timestamp")
		}
		if !consumed.StartedAt.Valid {
			t.Error("started_at is NULL, want a timestamp")
		}

		// Replay: the hash was cleared, so the same token returns no row.
		if _, err := q.ConsumeAuroraSandboxEnrollment(ctx, pgtype.Text{String: hash, Valid: true}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("replay consume = %v, want no rows", err)
		}

		// Expiry: a token past its expiry is not returned either.
		wsExpired, rtExpired := newSandboxNodeWorkspace(t, q, pool)
		expiredHash := auth.HashToken("mse_" + uuid.NewString())
		expired := sandboxNodeParams(t, wsExpired, rtExpired, "aurora-"+uuid.NewString())
		expired.EnrollmentTokenHash = pgtype.Text{String: expiredHash, Valid: true}
		expired.EnrollmentExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
		if _, err := q.CreateAuroraSandboxNode(ctx, expired); err != nil {
			t.Fatalf("create expired node: %v", err)
		}
		if _, err := q.ConsumeAuroraSandboxEnrollment(ctx, pgtype.Text{String: expiredHash, Valid: true}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("expired consume = %v, want no rows", err)
		}
	})
}
