package daemon

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/multica-ai/multica/server/internal/aurora"
)

// errAuroraSurfaceNotOnboarded is returned by runTask when an Aurora task is
// dispatched to a provider with no reviewed sandbox surface. The task must fail
// (and refund via the completion path) rather than run under the default
// autonomous surface.
var errAuroraSurfaceNotOnboarded = errors.New("aurora provider has no reviewed sandbox surface")

// errAuroraSurfaceUnknownSkill is returned when an Aurora task's trusted system
// key names no available skill. Like an un-onboarded provider, the task fails
// rather than inheriting a wider default surface.
var errAuroraSurfaceUnknownSkill = errors.New("aurora task has no reviewed skill workflow")

// auroraSystemKeyPrefix marks Aurora's workspace-level system agents. Their
// stable identity is "aurora:<skillID>" (see server/internal/aurora), which the
// claim endpoint forwards to the daemon as AgentData.SystemKey. The daemon reads
// that prefix to apply the sandbox execution policy below only to Aurora tasks —
// ordinary user agents keep their default autonomous surface.
const auroraSystemKeyPrefix = "aurora:"

// auroraExecutionProvider is the only provider with a reviewed Aurora sandbox
// surface, and the only execution identity a managed enrollment may install.
// The persisted runtime keeps its carrier identity (aurora_managed); execution
// comes from the enrollment envelope, never from the database row.
const auroraExecutionProvider = "claude"

// auroraMaxTurns caps how many agent turns a single Aurora generation may take.
// It is the MVP turn bound from Plan 3 Task 6; the daemon previously never set
// ExecOptions.MaxTurns even though the field and the per-provider --max-turns
// plumbing existed. A per-task call-count guard (keyed per task) is deferred to
// a follow-up (spec §10).
const auroraMaxTurns = 30

// auroraDisallowedTools is the fixed set of general-purpose tools every Aurora
// task runs without: shell and script execution (Bash, BashOutput, KillShell),
// arbitrary file tools (Read, Write, Edit, NotebookEdit, Glob, Grep), browser
// and network access (WebFetch, WebSearch), and the orchestration tools that
// could spawn a wider surface (Task, TodoWrite). The reviewed MCP broker is the
// only execution path an Aurora workflow may use.
var auroraDisallowedTools = []string{
	"Bash", "BashOutput", "KillShell",
	"Read", "Write", "Edit", "NotebookEdit",
	"Glob", "Grep",
	"WebFetch", "WebSearch",
	"Task", "TodoWrite",
}

// isAuroraTask reports whether the claimed task is an Aurora system-agent run.
func isAuroraTask(task Task) bool {
	return task.Agent != nil && strings.HasPrefix(task.Agent.SystemKey, auroraSystemKeyPrefix)
}

// auroraSkillID reads the skill id from the trusted system key the claim
// endpoint binds to the task-scoped token. It is never read from the prompt or
// from any other context-supplied tool list.
func auroraSkillID(task Task) (string, bool) {
	if task.Agent == nil {
		return "", false
	}
	skillID := strings.TrimSpace(strings.TrimPrefix(task.Agent.SystemKey, auroraSystemKeyPrefix))
	if skillID == "" || skillID == task.Agent.SystemKey {
		return "", false
	}
	return skillID, true
}

// auroraSurface is the narrowed tool surface an Aurora system agent runs under:
// a restricted permission mode (no bypass), the reviewed MCP tools the task may
// call, and the general-purpose tools it may not.
type auroraSurface struct {
	permissionMode string
	// allowed is the reviewed MCP tool set, derived only from the trusted skill
	// id and the server-owned execution policy — never from a prompt or context.
	allowed []string
	// disallowed is the fixed general-purpose deny set every Aurora task keeps.
	disallowed []string
}

// auroraToolSurface returns the narrowed surface for an Aurora system agent on
// provider. The narrowing has two parts: turn off bypass (so the deny list
// binds), then allow exactly the tools the trusted skill's policy requires
// while denying every general-purpose tool.
//
// It fails closed: only claude has a reviewed surface for the MVP, so any other
// provider returns errAuroraSurfaceNotOnboarded, and an unknown or unavailable
// skill returns errAuroraSurfaceUnknownSkill. aurora_managed is a persisted
// carrier identity, not an executable provider, so passing it through is an
// error too.
func auroraToolSurface(task Task, provider string) (auroraSurface, error) {
	if provider != auroraExecutionProvider {
		return auroraSurface{}, fmt.Errorf("%w: %s", errAuroraSurfaceNotOnboarded, provider)
	}
	skillID, ok := auroraSkillID(task)
	if !ok {
		return auroraSurface{}, fmt.Errorf("%w: %q", errAuroraSurfaceUnknownSkill, auroraSystemKey(task))
	}
	policy, ok := aurora.ExecutionPolicy(skillID)
	if !ok {
		return auroraSurface{}, fmt.Errorf("%w: %q", errAuroraSurfaceUnknownSkill, skillID)
	}
	return auroraSurface{
		permissionMode: "default",
		allowed:        slices.Clone(policy.RequiredTools),
		disallowed:     slices.Clone(auroraDisallowedTools),
	}, nil
}

// auroraSystemKey is a nil-safe read of the agent system key for diagnostics.
func auroraSystemKey(task Task) string {
	if task.Agent == nil {
		return ""
	}
	return task.Agent.SystemKey
}
