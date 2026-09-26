package aurora_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/aurora"
)

// auroraToolPattern extracts the MCP tool names a brief is allowed to call. The
// broker namespaces every reviewed tool as "aurora.<verb>", so this is the only
// tool-shaped token a brief may contain.
var auroraToolPattern = regexp.MustCompile("\\baurora\\.[a-z0-9_]+\\b")

// workflowForbiddenPatterns is the security half of the brief contract. A brief
// is model-facing prompt content: it may name reviewed tools and artifact IDs,
// but it must never hand the prompt a shell, a location, a secret, or a way to
// pick a vendor, a model, or a package.
var workflowForbiddenPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"URL", regexp.MustCompile("(?i)https?://")},
	{"shell command", regexp.MustCompile("(?i)\\b(bash|zsh|fish|sh|powershell)\\b")},
	{"dynamic package installation", regexp.MustCompile("(?i)\\b(npm|pnpm|yarn|pip3?|apt|apt-get|brew)\\s+(install|add|i|get)\\b")},
	{"npx invocation", regexp.MustCompile("(?i)\\bnpx\\b")},
	{"API key or secret", regexp.MustCompile("(?i)\\b(api[ _-]?key|secret|password|credential|bearer|token)\\b")},
	{"provider selection", regexp.MustCompile("(?i)\\b(provider|vendor|endpoint)\\b")},
	{"model selection", regexp.MustCompile("(?i)\\bmodel\\b")},
}

func availableSkills() []string {
	var ids []string
	for _, entry := range aurora.Catalog() {
		if entry.Available {
			ids = append(ids, entry.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// TestAvailableSkillsHaveCanonicalWorkflows asserts the embedded bundle carries
// exactly one brief per available catalog skill, that each brief names its
// skill, and that no brief smuggles in a shell, location, secret, or
// provider/model/package selection.
func TestAvailableSkillsHaveCanonicalWorkflows(t *testing.T) {
	available := availableSkills()
	if len(available) != 13 {
		t.Fatalf("available skills = %d, want 13", len(available))
	}

	// The directory listing itself is the "exactly one" ground truth: no
	// orphan brief may exist for an unavailable skill.
	entries, err := os.ReadDir("workflows")
	if err != nil {
		t.Fatalf("read workflows directory: %v", err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			t.Fatalf("workflows contains unexpected entry %q", entry.Name())
		}
		files = append(files, strings.TrimSuffix(entry.Name(), ".md"))
	}
	sort.Strings(files)
	if !slices.Equal(files, available) {
		t.Fatalf("workflow files = %v, want exactly %v", files, available)
	}

	for _, skill := range available {
		brief, ok := aurora.Workflow(skill)
		if !ok {
			t.Errorf("Workflow(%q) = missing, want a brief", skill)
			continue
		}
		if strings.TrimSpace(brief) == "" {
			t.Errorf("Workflow(%q) is empty", skill)
		}
		if !strings.Contains(brief, skill) {
			t.Errorf("Workflow(%q) does not name its skill", skill)
		}
		for _, rule := range workflowForbiddenPatterns {
			if match := rule.re.FindString(brief); match != "" {
				t.Errorf("Workflow(%q) contains %s text %q", skill, rule.name, match)
			}
		}
	}
}

// TestUnavailableSkillsHaveNoWorkflow keeps phase-2 skills non-executable: the
// three unavailable catalog entries and any unknown id must have no brief.
func TestUnavailableSkillsHaveNoWorkflow(t *testing.T) {
	var unavailable []string
	for _, entry := range aurora.Catalog() {
		if !entry.Available {
			unavailable = append(unavailable, entry.ID)
		}
	}
	sort.Strings(unavailable)
	want := []string{"avatar-video", "excel", "ppt"}
	if !slices.Equal(unavailable, want) {
		t.Fatalf("unavailable skills = %v, want %v", unavailable, want)
	}

	for _, id := range append(append([]string{}, want...), "", "unknown", "poster-2") {
		if brief, ok := aurora.Workflow(id); ok || brief != "" {
			t.Errorf("Workflow(%q) = (%q, %v), want no brief", id, brief, ok)
		}
	}
}

// TestWorkflowToolsMatchExecutionPolicy is the contract between the prompt and
// the server-owned policy: a brief may reference exactly the reviewed tools
// Task 1 recorded for its skill, and nothing outside the policy may appear in
// any brief.
func TestWorkflowToolsMatchExecutionPolicy(t *testing.T) {
	policyTools := map[string]bool{}
	for _, entry := range aurora.Catalog() {
		if !entry.Available {
			continue
		}
		policy, ok := aurora.ExecutionPolicy(entry.ID)
		if !ok {
			t.Fatalf("ExecutionPolicy(%q) not found", entry.ID)
		}
		if len(policy.RequiredTools) == 0 {
			t.Fatalf("ExecutionPolicy(%q) has no required tools", entry.ID)
		}
		for _, tool := range policy.RequiredTools {
			policyTools[tool] = true
		}
	}
	if len(policyTools) == 0 {
		t.Fatal("no execution policy declares required tools")
	}

	for _, entry := range aurora.Catalog() {
		if !entry.Available {
			continue
		}
		brief, ok := aurora.Workflow(entry.ID)
		if !ok {
			t.Fatalf("Workflow(%q) = missing", entry.ID)
		}
		policy, _ := aurora.ExecutionPolicy(entry.ID)

		referenced := auroraToolPattern.FindAllString(brief, -1)
		sort.Strings(referenced)
		referenced = slices.Compact(referenced)

		want := append([]string(nil), policy.RequiredTools...)
		sort.Strings(want)
		want = slices.Compact(want)
		if !slices.Equal(referenced, want) {
			t.Errorf("Workflow(%q) tools = %v, want exactly %v", entry.ID, referenced, want)
		}
		for _, tool := range referenced {
			if !policyTools[tool] {
				t.Errorf("Workflow(%q) references %q, which no execution policy declares", entry.ID, tool)
			}
		}
	}
}
