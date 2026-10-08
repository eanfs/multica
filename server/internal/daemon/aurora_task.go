package daemon

import (
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/aurora"
)

// auroraSystemKeyPrefix marks Aurora's workspace-level system agents. Their
// stable identity is "aurora:<skillID>" (see server/internal/aurora), which the
// claim endpoint forwards to the daemon as AgentData.SystemKey.
const auroraSystemKeyPrefix = "aurora:"

// Required links remain editable. Fail delivery before launching the model if
// removal, disabling, deletion or an empty document leaves the skill unavailable.
// Match the workspace seed's catalog name, not the editable agent display name.
func requireAuroraSkillDocument(task Task) error {
	if !isAuroraTask(task) {
		return nil
	}
	skillID, _ := auroraSkillID(task)
	entry, ok := aurora.Lookup(skillID)
	if ok {
		for _, skill := range task.Agent.Skills {
			if skill.Source == "workspace" && skill.Name == entry.Name && strings.TrimSpace(skill.Content) != "" {
				return nil
			}
		}
	}
	return fmt.Errorf("required Aurora skill document for %q is missing or empty; restore and enable the agent's catalog skill before retrying", skillID)
}

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
