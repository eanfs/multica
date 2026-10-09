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

// auroraManifestSchema is the one "aurora.<name>" token a skill document may
// contain. The deleted broker namespaced every reviewed tool as "aurora.<verb>"
// and named them in the briefs; an Aurora task now runs on the ordinary agent
// surface, so a document must carry its own shell steps instead. This schema
// name is not a tool, which is why it is the sole exception.
const auroraManifestSchema = "com.multica.aurora.artifacts"

// auroraToolPattern extracts tool-shaped tokens from a brief. Only
// auroraManifestSchema's tail may match.
var auroraToolPattern = regexp.MustCompile(`\baurora\.[a-z0-9_]+\b`)

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
// exactly one brief per available catalog skill and that each brief names its
// skill. What a brief may say is TestRewrittenWorkflowsDescribeRealSteps.
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
// the server-owned policy. The policy's RequiredTools used to be the daemon's
// tool allowlist, so the older form of this test asserted that a brief named
// exactly them; the daemon no longer reads the field (the broker is gone), but
// the table still names the vendored producer each route runs on. A brief must
// name none of them: an Aurora document reaches its provider through the shell,
// not through a brokered tool.
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
		if policy.Route == "" {
			t.Fatalf("ExecutionPolicy(%q) has no route", entry.ID)
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
		for tool := range policyTools {
			if strings.Contains(brief, tool) {
				t.Errorf("workflow %q still names broker tool %q", entry.ID, tool)
			}
		}
	}
}

// TestRewrittenWorkflowsDescribeRealSteps is the contract for a brief an
// ordinary agent can execute. Every available skill is held to it: the brief
// carries the five fixed sections, describes real steps, and never falls back
// to the deleted broker's vocabulary.
func TestRewrittenWorkflowsDescribeRealSteps(t *testing.T) {
	for _, id := range availableSkills() {
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
		// The banned list only names the tools that existed. A rewritten tool
		// name would slip past it, and past the policy names too, so match the
		// namespace itself and allow only the manifest schema.
		for _, ref := range auroraToolPattern.FindAllString(brief, -1) {
			if ref != "aurora.artifacts" {
				t.Errorf("skill %q names %q: that is a broker tool name, not a step", id, ref)
			}
		}
		if !strings.Contains(brief, auroraManifestSchema) {
			t.Errorf("skill %q does not name the artifact manifest schema %q", id, auroraManifestSchema)
		}
		for _, want := range []string{"## Inputs", "## Steps", "## Required outputs", "## Artifact manifest", "## Failure behavior"} {
			if !strings.Contains(brief, want) {
				t.Errorf("skill %q is missing the %q section", id, want)
			}
		}
	}
}

// TestEveryAvailableSkillHasADocument keeps the catalog and the embedded bundle
// in step.
func TestEveryAvailableSkillHasADocument(t *testing.T) {
	for _, e := range aurora.Catalog() {
		if _, ok := aurora.Workflow(e.ID); e.Available && !ok {
			t.Errorf("available skill %q has no workflow document", e.ID)
		}
	}
}
