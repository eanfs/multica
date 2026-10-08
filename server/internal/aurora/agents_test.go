package aurora_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/testutil"
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
		policy, ok := aurora.ExecutionPolicy(d.SkillID)
		if !ok {
			if len(d.RequiredTools) != 0 {
				t.Errorf("unavailable system agent %q has required tools %v", d.SkillID, d.RequiredTools)
			}
		} else if !slices.Equal(d.RequiredTools, policy.RequiredTools) {
			t.Errorf("system agent %q required tools = %v, want %v", d.SkillID, d.RequiredTools, policy.RequiredTools)
		}
		keys[d.SkillID] = true
	}
	for _, c := range cat {
		if !keys[c.ID] {
			t.Fatalf("catalog skill %q has no system agent", c.ID)
		}
	}
}

// Uses the same fixture availability gate as the existing DB tests.
func TestAuroraAgentsAreVisibleToMembers(t *testing.T) {
	if agentsTestPool == nil {
		t.Skip("database not available")
	}
	placeholders := seedUnavailableAgents(t)
	ctx := context.Background()
	tx, err := agentsTestPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	ws, owner := util.MustParseUUID(agentsTestWorkspaceID), util.MustParseUUID(agentsTestUserID)
	if err := aurora.EnsureSystemAgents(ctx, q, ws, owner); err != nil {
		t.Fatal(err)
	}
	agents, err := q.ListAgents(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]db.Agent{}
	for _, agent := range agents {
		if strings.HasPrefix(agent.SystemKey.String, "aurora:") {
			byKey[agent.SystemKey.String] = agent
		}
	}
	if len(byKey) != 13 {
		t.Fatalf("visible Aurora agents = %d, want 13", len(byKey))
	}
	available := 0
	for _, entry := range aurora.Catalog() {
		agent, ok := byKey["aurora:"+entry.ID]
		if !entry.Available {
			if ok {
				t.Fatalf("unavailable agent %s is visible", entry.ID)
			}
			continue
		}
		if !ok || agent.Kind != "user" || agent.Name != entry.Name {
			t.Fatalf("catalog agent %s not visible: %+v", entry.ID, agent)
		}
		if entry.Available {
			available++
		}
		if _, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agent.ID, WorkspaceID: ws}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agent.ID, WorkspaceID: owner}); err == nil {
			t.Fatal("cross-workspace agent leaked")
		}
		skills, err := q.ListAgentSkills(ctx, agent.ID)
		if err != nil || len(skills) != 1 || skills[0].Name != entry.Name {
			t.Fatalf("catalog skill not linked: %v %v", skills, err)
		}
	}
	if available != 13 {
		t.Fatalf("available visible agents = %d, want 13", available)
	}
	// Simulate a previously seeded hidden, archived agent. Reseeding must repair
	// it in place, preserving the identity used by generation lookup.
	original := byKey["aurora:text-image"]
	if _, err := tx.Exec(ctx, "UPDATE agent SET kind = 'system', archived_at = now(), archived_by = $2 WHERE id = $1", original.ID, owner); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := aurora.EnsureSystemAgents(ctx, q, ws, owner); err != nil {
			t.Fatal(err)
		}
		got, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: original.ID, WorkspaceID: ws})
		if err != nil || got.ArchivedAt.Valid || got.ArchivedBy.Valid || got.RuntimeID != original.RuntimeID || got.SystemKey != original.SystemKey {
			t.Fatalf("seed did not restore identity: %+v %v", got, err)
		}
		for id, before := range placeholders {
			var after string
			if err := tx.QueryRow(ctx, "SELECT to_jsonb(agent)::text FROM agent WHERE id = $1", id).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("unavailable agent %s changed: before %s after %s", id, before, after)
			}
		}
	}
}

func TestAuroraAgentVisibilityMigrationIsIdempotent(t *testing.T) {
	if agentsTestPool == nil {
		t.Skip("database not available")
	}
	dbfx := testutil.New(agentsTestPool, agentsTestWorkspaceID, agentsTestUserID)
	ordinaryID := dbfx.Insert(t, "agent", testutil.Cols{
		"workspace_id": agentsTestWorkspaceID, "owner_id": agentsTestUserID,
		"name": "Ordinary member agent", "kind": "user", "runtime_mode": "cloud",
		"system_key":     "aurora:future-unlisted",
		"runtime_config": testutil.Raw("'{}'::jsonb"), "visibility": "workspace",
	})
	placeholders := seedUnavailableAgents(t)
	ctx := context.Background()
	tx, err := agentsTestPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	ws, owner := util.MustParseUUID(agentsTestWorkspaceID), util.MustParseUUID(agentsTestUserID)
	if err := aurora.EnsureSystemAgents(ctx, q, ws, owner); err != nil {
		t.Fatal(err)
	}
	before, err := q.GetAgentBySystemKey(ctx, db.GetAgentBySystemKeyParams{WorkspaceID: ws, SystemKey: pgtype.Text{String: "aurora:text-image", Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE agent SET kind = 'system' WHERE workspace_id = $1 AND kind = 'user' AND system_key LIKE 'aurora:%' AND id != $2", ws, util.MustParseUUID(ordinaryID)); err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile("../../migrations/585_aurora_agents_visible.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		tag, err := tx.Exec(ctx, string(up))
		if err != nil {
			t.Fatal(err)
		}
		for id, before := range placeholders {
			var after string
			if err := tx.QueryRow(ctx, "SELECT to_jsonb(agent)::text FROM agent WHERE id = $1", id).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("unavailable agent %s changed: before %s after %s", id, before, after)
			}
		}
		if i == 1 && tag.RowsAffected() != 0 {
			t.Fatalf("second migration changed %d rows", tag.RowsAffected())
		}
	}
	after, err := q.GetAgentBySystemKey(ctx, db.GetAgentBySystemKeyParams{WorkspaceID: ws, SystemKey: before.SystemKey})
	if err != nil || after.ID != before.ID || after.RuntimeID != before.RuntimeID || after.Kind != "user" {
		t.Fatalf("migration changed identity or hid agent: %+v %v", after, err)
	}
	visible, err := q.ListAgents(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, agent := range visible {
		if strings.HasPrefix(agent.SystemKey.String, "aurora:") && agent.ID != util.MustParseUUID(ordinaryID) {
			count++
		}
	}
	if count != 13 {
		t.Fatalf("migration exposed %d catalog agents, want 13", count)
	}
	down, err := os.ReadFile("../../migrations/585_aurora_agents_visible.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		tag, err := tx.Exec(ctx, string(down))
		if err != nil {
			t.Fatal(err)
		}
		for id, before := range placeholders {
			var after string
			if err := tx.QueryRow(ctx, "SELECT to_jsonb(agent)::text FROM agent WHERE id = $1", id).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("unavailable agent %s changed: before %s after %s", id, before, after)
			}
		}
		if i == 1 && tag.RowsAffected() != 0 {
			t.Fatal("down migration is not idempotent")
		}
	}
	after, err = q.GetAgentBySystemKey(ctx, db.GetAgentBySystemKeyParams{WorkspaceID: ws, SystemKey: before.SystemKey})
	if err != nil || after.ID != before.ID || after.Kind != "system" {
		t.Fatalf("down migration: %+v %v", after, err)
	}
	ordinary, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: util.MustParseUUID(ordinaryID), WorkspaceID: ws})
	if err != nil || ordinary.Kind != "user" || ordinary.Name != "Ordinary member agent" {
		t.Fatalf("ordinary agent changed: %+v %v", ordinary, err)
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
	assertSkillContentMatchesWorkflow(t, ws)
}

// assertSkillContentMatchesWorkflow proves the seed writes each available
// skill's embedded brief to the skill row without reading a vendor SKILL.md.
func assertSkillContentMatchesWorkflow(t *testing.T, ws pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	for _, entry := range aurora.Catalog() {
		if !entry.Available {
			continue
		}
		var content string
		if err := agentsTestPool.QueryRow(ctx,
			`SELECT content FROM skill WHERE workspace_id = $1 AND name = $2`, ws, entry.Name).Scan(&content); err != nil {
			t.Fatalf("load skill %q content: %v", entry.ID, err)
		}
		brief, ok := aurora.Workflow(entry.ID)
		if !ok {
			if brief != "" {
				t.Errorf("Workflow(%q) = %q, want no brief", entry.ID, brief)
			}
			continue
		}
		if content != brief {
			t.Errorf("skill %q content does not match the embedded workflow", entry.ID)
		}
	}
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
		`SELECT count(*) FROM agent WHERE workspace_id = $1 AND kind = 'user' AND system_key LIKE 'aurora:%'`, ws).Scan(&agentCount); err != nil {
		t.Fatalf("count system agents: %v", err)
	}
	if agentCount != 13 {
		t.Errorf("system agent rows = %d, want 13", agentCount)
	}

	var skillCount int
	if err := agentsTestPool.QueryRow(ctx,
		`SELECT count(*) FROM skill WHERE workspace_id = $1`, ws).Scan(&skillCount); err != nil {
		t.Fatalf("count skills: %v", err)
	}
	if skillCount != 13 {
		t.Errorf("skill rows = %d, want 13", skillCount)
	}

	var junctionCount int
	if err := agentsTestPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_skill
		WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1 AND kind = 'user' AND system_key LIKE 'aurora:%')`,
		ws).Scan(&junctionCount); err != nil {
		t.Fatalf("count agent_skill: %v", err)
	}
	if junctionCount != 13 {
		t.Errorf("agent_skill rows = %d, want 13", junctionCount)
	}
}

// Seed legacy hidden placeholders and capture their complete row state.
func seedUnavailableAgents(t *testing.T) map[string]string {
	t.Helper()
	dbfx := testutil.New(agentsTestPool, agentsTestWorkspaceID, agentsTestUserID)
	snapshots := map[string]string{}
	for _, key := range []string{"aurora:avatar-video", "aurora:ppt", "aurora:excel"} {
		id := dbfx.Insert(t, "agent", testutil.Cols{
			"workspace_id": agentsTestWorkspaceID, "owner_id": agentsTestUserID,
			"name": "Preserved " + key, "kind": "system", "system_key": key,
			"runtime_mode": "cloud", "runtime_config": testutil.Raw("'{}'::jsonb"), "visibility": "workspace",
		})
		var snapshot string
		if err := agentsTestPool.QueryRow(context.Background(), "SELECT to_jsonb(agent)::text FROM agent WHERE id = $1", id).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		snapshots[id] = snapshot
	}
	return snapshots
}
