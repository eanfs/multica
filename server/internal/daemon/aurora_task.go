package daemon

import "strings"

// auroraSystemKeyPrefix marks Aurora's workspace-level system agents. Their
// stable identity is "aurora:<skillID>" (see server/internal/aurora), which the
// claim endpoint forwards to the daemon as AgentData.SystemKey.
const auroraSystemKeyPrefix = "aurora:"

// auroraExecutionProvider is the only execution identity a managed enrollment
// may install. The persisted runtime keeps its carrier identity
// (aurora_managed); execution comes from the enrollment envelope, never from the
// database row.
const auroraExecutionProvider = "claude"

// isAuroraTask reports whether the claimed task is an Aurora system-agent run.
//
// An Aurora task executes exactly like any other task: same workdir, same
// provider CLI, same tool surface. The prefix survives because the artifact
// contract still needs the skill identity — attachAuroraArtifacts validates the
// run's manifest against the skill the task claims to be (aurora_manifest.go).
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

// auroraSystemKey is a nil-safe read of the agent system key for diagnostics.
func auroraSystemKey(task Task) string {
	if task.Agent == nil {
		return ""
	}
	return task.Agent.SystemKey
}
