package daemon

import "strings"

// auroraSystemKeyPrefix marks Aurora's workspace-level system agents. Their
// stable identity is "aurora:<skillID>" (see server/internal/aurora), which the
// claim endpoint now forwards to the daemon as AgentData.SystemKey. The daemon
// reads that prefix to apply the sandbox execution policy below only to Aurora
// tasks — ordinary user agents keep their default autonomous surface.
const auroraSystemKeyPrefix = "aurora:"

// auroraMaxTurns caps how many agent turns a single Aurora generation may take.
// It is the MVP turn bound from Plan 3 Task 6; the daemon previously never set
// ExecOptions.MaxTurns even though the field and the per-provider --max-turns
// plumbing existed. A per-task call-count guard (keyed per task) is deferred to
// a follow-up (spec §10).
const auroraMaxTurns = 30

// isAuroraTask reports whether the claimed task is an Aurora system-agent run.
func isAuroraTask(task Task) bool {
	return task.Agent != nil && strings.HasPrefix(task.Agent.SystemKey, auroraSystemKeyPrefix)
}

// auroraToolSurface returns the narrowed tool surface for an Aurora system
// agent, keyed by provider. The narrowing has two parts: turning off bypass so
// the deny list actually binds, and denying the host-touching tools that
// provider exposes. The empty permission mode means "keep the backend default",
// and an empty deny list means "no additional narrowing".
//
// Only the claude surface is defined for the MVP: it is the coding agent the
// sandbox node is validated against. The allowlist is a deny-list first pass —
// the per-skill allow/deny composition and the equivalents for codex and the
// other providers are refined as each is onboarded onto the sandbox image
// (Plan 3 follow-up). A provider that returns no narrowing here is assumed not
// to be installed on the sandbox node (see the sandbox image docs), so leaving
// it un-narrowed must not be read as "safe on the host".
func auroraToolSurface(provider string) (permissionMode string, disallowed []string) {
	switch provider {
	case "claude":
		return "default", []string{"Bash", "WebFetch", "WebSearch"}
	default:
		return "", nil
	}
}
