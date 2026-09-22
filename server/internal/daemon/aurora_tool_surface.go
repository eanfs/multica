package daemon

import (
	"errors"
	"strings"
)

// errAuroraSurfaceNotOnboarded is returned by runTask when an Aurora task is
// dispatched to a provider with no reviewed sandbox surface. The task must fail
// (and refund via the completion path) rather than run under the default
// autonomous surface.
var errAuroraSurfaceNotOnboarded = errors.New("aurora provider has no reviewed sandbox surface")

// auroraSystemKeyPrefix marks Aurora's workspace-level system agents. Their
// stable identity is "aurora:<skillID>" (see server/internal/aurora), which the
// claim endpoint forwards to the daemon as AgentData.SystemKey. The daemon reads
// that prefix to apply the sandbox execution policy below only to Aurora tasks —
// ordinary user agents keep their default autonomous surface.
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

// auroraSurface is the narrowed tool surface an Aurora system agent runs under:
// a restricted permission mode (no bypass) plus the host-touching tools to deny.
type auroraSurface struct {
	permissionMode string
	disallowed     []string
}

// auroraToolSurface returns the narrowed surface for an Aurora system agent on
// provider, and whether that provider has a reviewed surface. The narrowing has
// two parts: turn off bypass (so the deny list binds) and deny the host-touching
// tools the provider exposes.
//
// It fails closed: only claude has a reviewed surface for the MVP, so any other
// provider reports ok=false and the caller must refuse the task rather than fall
// back to the default autonomous (bypassPermissions) surface. The per-skill
// allow/deny composition and codex/other-provider equivalents are refined as
// each is onboarded onto the sandbox image (Plan 3 follow-up).
func auroraToolSurface(provider string) (auroraSurface, bool) {
	switch provider {
	case "claude":
		return auroraSurface{permissionMode: "default", disallowed: []string{"Bash", "WebFetch", "WebSearch"}}, true
	default:
		return auroraSurface{}, false
	}
}
