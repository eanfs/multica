package aurora

import (
	"embed"
	"path"
	"strings"
)

// workflowBundle holds one canonical Markdown brief per available skill. The
// filename is the skill ID, and the bundle intentionally contains no brief for
// avatar-video, ppt, or excel, which stay unavailable.
//
//go:embed workflows/*.md
var workflowBundle embed.FS

// Workflow returns the canonical, model-facing brief for an available skill.
// Availability is decided by the same execution policy that gates a task, so an
// unavailable or unknown skill can never receive a brief. The returned string
// is a copy the caller owns.
func Workflow(skillID string) (string, bool) {
	if _, ok := ExecutionPolicy(skillID); !ok {
		return "", false
	}
	content, err := workflowBundle.ReadFile(path.Join("workflows", skillID+".md"))
	if err != nil {
		return "", false
	}
	brief := strings.TrimSpace(string(content))
	if brief == "" {
		return "", false
	}
	return brief, true
}
