package aurora

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// SystemAgentDef is one workspace-level system agent carrying a single Aurora
// skill. The 16 definitions correspond one-to-one with the 16 catalog entries;
// SystemKey is the agent's stable identity ("aurora:"+skillID), never its
// display name.
type SystemAgentDef struct {
	SkillID      string
	SystemKey    string
	Name         string
	Instructions string
}

// SystemAgents returns the 16 system-agent definitions in catalog order.
func SystemAgents() []SystemAgentDef {
	cat := Catalog()
	defs := make([]SystemAgentDef, len(cat))
	for i, e := range cat {
		defs[i] = systemAgentDef(e)
	}
	return defs
}

func systemAgentDef(e SkillCatalogEntry) SystemAgentDef {
	return SystemAgentDef{
		SkillID:      e.ID,
		SystemKey:    "aurora:" + e.ID,
		Name:         e.Name,
		Instructions: systemAgentInstructions(e),
	}
}

// The managed runtime is the workspace's server-hosted host for Aurora's
// system agents. provider distinguishes it from user daemon-registered
// runtimes; nothing else keys off the value yet, task routing will read the
// agent's provider at claim time (Plan 3 Task 3).
const (
	managedRuntimeProvider = "aurora_managed"
	managedRuntimeName     = "Aurora Managed Runtime"
)

// EnsureSystemAgents lazily materialises Aurora's 16 workspace-level system
// agents: one managed runtime row, and per catalog skill one kind='system'
// agent, one skill row, and the agent_skill junction. It is idempotent —
// calling it twice against the same workspace leaves the same rows — so the
// generation-creation path (Plan 3 Task 2) can seed every workspace without
// counting rows first.
//
// ownerID must be a real user id (agent.owner_id references "user"): the
// caller passes the workspace owner. No transaction is taken on purpose: each
// statement is independently idempotent, so a partially-applied seed converges
// on retry.
func EnsureSystemAgents(ctx context.Context, q *db.Queries, workspaceID, ownerID pgtype.UUID) error {
	runtimeID, err := ensureManagedRuntime(ctx, q, workspaceID, ownerID)
	if err != nil {
		return err
	}
	for _, e := range Catalog() {
		skill, err := q.UpsertAuroraSkill(ctx, db.UpsertAuroraSkillParams{
			WorkspaceID: workspaceID,
			Name:        e.Name,
			Content:     systemSkillContent(e),
			CreatedBy:   ownerID,
		})
		if err != nil {
			return fmt.Errorf("upsert skill %q: %w", e.ID, err)
		}
		def := systemAgentDef(e)
		agent, err := q.UpsertAuroraSystemAgent(ctx, db.UpsertAuroraSystemAgentParams{
			WorkspaceID:  workspaceID,
			OwnerID:      ownerID,
			RuntimeID:    runtimeID,
			SystemKey:    pgtype.Text{String: def.SystemKey, Valid: true},
			Name:         def.Name,
			Instructions: def.Instructions,
		})
		if err != nil {
			return fmt.Errorf("upsert agent %q: %w", e.ID, err)
		}
		if err := q.AddAgentSkill(ctx, db.AddAgentSkillParams{
			AgentID: agent.ID,
			SkillID: skill.ID,
		}); err != nil {
			return fmt.Errorf("link agent %q: %w", e.ID, err)
		}
	}
	return nil
}

// ensureManagedRuntime returns the workspace's managed runtime, creating it on
// first seed. Lookup-then-insert is the idempotency strategy: the runtime's
// NULL daemon_id sits outside migration 121's partial unique index (NULLs are
// distinct), so there is no ON CONFLICT arbiter to lean on. Concurrent seeds
// could mint a second row; the call site serialises on a per-workspace lock
// (Plan 3 Task 2), which is where that guarantee belongs.
func ensureManagedRuntime(ctx context.Context, q *db.Queries, workspaceID, ownerID pgtype.UUID) (pgtype.UUID, error) {
	rt, err := q.GetAuroraManagedRuntime(ctx, db.GetAuroraManagedRuntimeParams{
		WorkspaceID: workspaceID,
		Provider:    managedRuntimeProvider,
	})
	if err == nil {
		return rt.ID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, err
	}
	created, err := q.CreateAuroraManagedRuntime(ctx, db.CreateAuroraManagedRuntimeParams{
		WorkspaceID: workspaceID,
		Name:        managedRuntimeName,
		Provider:    managedRuntimeProvider,
		OwnerID:     ownerID,
	})
	if err != nil {
		return pgtype.UUID{}, err
	}
	return created.ID, nil
}

// systemAgentInstructions is the minimal system prompt for a skill's system
// agent. Skeletal on purpose: the real prompt is the runtime brief the claim
// path composes, and Plan 3.5 enriches these into full workflows.
func systemAgentInstructions(e SkillCatalogEntry) string {
	return fmt.Sprintf(
		"You are the Aurora %s agent (skill %q). Produce %s output from %s input.",
		e.NameEn, e.ID, strings.Join(e.Output, " / "), strings.Join(e.Input, " / "),
	)
}

// systemSkillContent is the minimal SKILL.md workflow text stored on the skill
// row. Later plans enrich it; the seed refreshes it on every run.
func systemSkillContent(e SkillCatalogEntry) string {
	return fmt.Sprintf("# %s\n\nAurora skill %q.\nInput: %s\nOutput: %s\n",
		e.Name, e.ID, strings.Join(e.Input, ", "), strings.Join(e.Output, ", "))
}
