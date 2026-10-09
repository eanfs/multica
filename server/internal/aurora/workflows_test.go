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

// workflowForbiddenPatterns is the security half of the broker-form brief
// contract. A broker-form brief is model-facing prompt content: it may name
// reviewed tools and artifact IDs, but it must never hand the prompt a shell, a
// location, a secret, or a way to pick a vendor, a model, or a package. A
// rewritten brief (see rewrittenSkills) deliberately carries the real commands,
// endpoints, and credential variable names, so these rules do not apply to it.
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

// rewrittenSkills lists the skill documents that have been converted to the
// ordinary-agent form. It grew one skill at a time, each verified end to end
// before the next was written, and now covers every available skill: a document
// that still tells the model to call a brokered MCP tool is a bug, not a stale
// comment. A skill added to the catalog later must be added here too, or
// TestAvailableSkillsHaveCanonicalWorkflows will hold it to the broker-form
// contract instead.
var rewrittenSkills = []string{
	"document-summary",
	"id-photo",
	"image-edit",
	"image-video",
	"poster",
	"product-image",
	"resume",
	"text-image",
	"text-video",
	"transcription",
	"video-captions",
	"xhs-copy",
	"xhs-image",
}

// rewrittenSkillSet is the lookup form of rewrittenSkills.
func rewrittenSkillSet() map[string]bool {
	set := make(map[string]bool, len(rewrittenSkills))
	for _, id := range rewrittenSkills {
		set[id] = true
	}
	return set
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
// skill, and that no broker-form brief smuggles in a shell, location, secret, or
// provider/model/package selection. Rewritten briefs carry those facts on
// purpose and are checked by TestRewrittenWorkflowsDescribeRealSteps instead.
func TestAvailableSkillsHaveCanonicalWorkflows(t *testing.T) {
	available := availableSkills()
	rewritten := rewrittenSkillSet()
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
		if rewritten[skill] {
			// A rewritten brief is the executable form; its contract is
			// TestRewrittenWorkflowsDescribeRealSteps.
			continue
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
// the server-owned policy: a broker-form brief may reference exactly the
// reviewed tools Task 1 recorded for its skill, and nothing outside the policy
// may appear in any broker-form brief. A rewritten brief must name no brokered
// tool at all, because it reaches the provider through the shell.
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

	rewritten := rewrittenSkillSet()
	for _, entry := range aurora.Catalog() {
		if !entry.Available {
			continue
		}
		brief, ok := aurora.Workflow(entry.ID)
		if !ok {
			t.Fatalf("Workflow(%q) = missing", entry.ID)
		}
		policy, _ := aurora.ExecutionPolicy(entry.ID)

		if rewritten[entry.ID] {
			// A rewritten brief must name no brokered tool. The loose
			// aurora.<verb> pattern cannot decide that: every rewritten manifest
			// carries the schema name "com.multica.aurora.artifacts", which is
			// not a tool. Compare against the policy's own tool names instead.
			for tool := range policyTools {
				if strings.Contains(brief, tool) {
					t.Errorf("rewritten workflow %q still names broker tool %q", entry.ID, tool)
				}
			}
			continue
		}

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

// TestRewrittenWorkflowsDescribeRealSteps is the contract for a brief an
// ordinary agent can execute: it carries the five fixed sections and never
// tells the model to call a brokered MCP tool or claims the runtime has no
// shell.
func TestRewrittenWorkflowsDescribeRealSteps(t *testing.T) {
	for _, id := range rewrittenSkills {
		brief, ok := aurora.Workflow(id)
		if !ok {
			t.Fatalf("skill %q has no workflow document", id)
		}
		for _, banned := range []string{"mcp__aurora__", "aurora.seedream_generate", "aurora.seedance_generate",
			"aurora.openai_image", "aurora.volc_asr_transcribe", "aurora.read_document", "aurora.id_photo",
			"aurora.render_video_captions", "aurora.render_resume", "aurora.write_text_artifact",
			"MCP broker", "brokered tool", "no shell"} {
			if strings.Contains(brief, banned) {
				t.Errorf("skill %q still references %q", id, banned)
			}
		}
		for _, want := range []string{"## Inputs", "## Steps", "## Required outputs", "## Artifact manifest", "## Failure behavior"} {
			if !strings.Contains(brief, want) {
				t.Errorf("skill %q is missing the %q section", id, want)
			}
		}
	}
}

// TestEveryAvailableSkillHasADocument keeps the catalog and the embedded bundle
// in step. It does not require the document to be rewritten yet.
func TestEveryAvailableSkillHasADocument(t *testing.T) {
	for _, e := range aurora.Catalog() {
		if _, ok := aurora.Workflow(e.ID); e.Available && !ok {
			t.Errorf("available skill %q has no workflow document", e.ID)
		}
	}
}
