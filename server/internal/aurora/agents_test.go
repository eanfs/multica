package aurora_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	agentsTestPool        *pgxpool.Pool
	agentsTestWorkspaceID string
	agentsTestUserID      string
)

const (
	agentsFixtureEmail = "aurora-agents-fixture@multica.ai"
	agentsFixtureSlug  = "aurora-agents-fixture"
)

// TestMain builds the fixture workspace the DB test hangs off. Without a
// database the suite exits green rather than red, the same contract every
// other DB-backed package here follows: the pure unit test above still runs on
// a laptop with no Postgres.
func TestMain(m *testing.M) {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		fmt.Printf("Skipping aurora DB tests: could not connect: %v\n", err)
		os.Exit(m.Run())
	}
	if err := pool.Ping(ctx); err != nil {
		fmt.Printf("Skipping aurora DB tests: database not reachable: %v\n", err)
		pool.Close()
		os.Exit(m.Run())
	}
	if err := seedAgentsFixture(ctx, pool); err != nil {
		fmt.Printf("Skipping aurora DB tests: seed failed: %v\n", err)
		pool.Close()
		os.Exit(m.Run())
	}
	agentsTestPool = pool

	code := m.Run()
	_, _ = pool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, agentsFixtureSlug)
	_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, agentsFixtureEmail)
	pool.Close()
	os.Exit(code)
}

func seedAgentsFixture(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, agentsFixtureSlug); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, agentsFixtureEmail); err != nil {
		return err
	}
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Aurora Agents Fixture User", agentsFixtureEmail).Scan(&agentsTestUserID); err != nil {
		return err
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, issue_prefix) VALUES ($1, $2, $3) RETURNING id`,
		"Aurora Agents Fixture", agentsFixtureSlug, "AUR").Scan(&agentsTestWorkspaceID); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		agentsTestWorkspaceID, agentsTestUserID)
	return err
}

func TestSystemAgentsMatchCatalog(t *testing.T) {
	defs := aurora.SystemAgents()
	cat := aurora.Catalog()
	if len(defs) != len(cat) {
		t.Fatalf("system agents %d != catalog %d", len(defs), len(cat))
	}
	keys := map[string]bool{}
	for _, d := range defs {
		if d.SystemKey != "aurora:"+d.SkillID {
			t.Fatalf("system key %q != aurora:%s", d.SystemKey, d.SkillID)
		}
		if d.Name == "" || d.Instructions == "" {
			t.Errorf("system agent %q missing name or instructions", d.SkillID)
		}
		keys[d.SkillID] = true
	}
	for _, c := range cat {
		if !keys[c.ID] {
			t.Fatalf("catalog skill %q has no system agent", c.ID)
		}
	}
}

func TestEnsureSystemAgents(t *testing.T) {
	if agentsTestPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	q := db.New(agentsTestPool)
	ws := util.MustParseUUID(agentsTestWorkspaceID)
	owner := util.MustParseUUID(agentsTestUserID)

	if err := aurora.EnsureSystemAgents(ctx, q, ws, owner); err != nil {
		t.Fatalf("EnsureSystemAgents: %v", err)
	}
	assertSeeded(t, ws)

	// Idempotency: a second call must not add rows.
	if err := aurora.EnsureSystemAgents(ctx, q, ws, owner); err != nil {
		t.Fatalf("EnsureSystemAgents (retry): %v", err)
	}
	assertSeeded(t, ws)
}

func assertSeeded(t *testing.T, ws pgtype.UUID) {
	t.Helper()
	ctx := context.Background()

	var runtimeCount int
	if err := agentsTestPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_runtime
		WHERE workspace_id = $1 AND daemon_id IS NULL AND runtime_mode = 'cloud' AND provider = 'aurora_managed'`,
		ws).Scan(&runtimeCount); err != nil {
		t.Fatalf("count managed runtime: %v", err)
	}
	if runtimeCount != 1 {
		t.Fatalf("managed runtime rows = %d, want 1", runtimeCount)
	}

	var runtimeMode string
	var daemonID pgtype.Text
	if err := agentsTestPool.QueryRow(ctx, `
		SELECT runtime_mode, daemon_id FROM agent_runtime
		WHERE workspace_id = $1 AND daemon_id IS NULL AND runtime_mode = 'cloud' AND provider = 'aurora_managed'
		LIMIT 1`, ws).Scan(&runtimeMode, &daemonID); err != nil {
		t.Fatalf("load managed runtime: %v", err)
	}
	if runtimeMode != "cloud" {
		t.Errorf("managed runtime runtime_mode = %q, want cloud", runtimeMode)
	}
	if daemonID.Valid {
		t.Errorf("managed runtime daemon_id = %q, want NULL", daemonID.String)
	}

	var agentCount int
	if err := agentsTestPool.QueryRow(ctx,
		`SELECT count(*) FROM agent WHERE workspace_id = $1 AND kind = 'system'`, ws).Scan(&agentCount); err != nil {
		t.Fatalf("count system agents: %v", err)
	}
	if agentCount != 16 {
		t.Errorf("system agent rows = %d, want 16", agentCount)
	}

	var skillCount int
	if err := agentsTestPool.QueryRow(ctx,
		`SELECT count(*) FROM skill WHERE workspace_id = $1`, ws).Scan(&skillCount); err != nil {
		t.Fatalf("count skills: %v", err)
	}
	if skillCount != 16 {
		t.Errorf("skill rows = %d, want 16", skillCount)
	}

	var junctionCount int
	if err := agentsTestPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_skill
		WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1 AND kind = 'system')`,
		ws).Scan(&junctionCount); err != nil {
		t.Fatalf("count agent_skill: %v", err)
	}
	if junctionCount != 16 {
		t.Errorf("agent_skill rows = %d, want 16", junctionCount)
	}
}
