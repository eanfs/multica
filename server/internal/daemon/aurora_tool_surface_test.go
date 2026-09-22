package daemon

import (
	"slices"
	"testing"
)

func TestIsAuroraTask(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		agent     *AgentData
		wantAurora bool
	}{
		{name: "aurora system agent", agent: &AgentData{SystemKey: "aurora:image"}, wantAurora: true},
		{name: "aurora skill key", agent: &AgentData{SystemKey: "aurora:video-generate"}, wantAurora: true},
		{name: "mika built-in", agent: &AgentData{SystemKey: "mika"}, wantAurora: false},
		{name: "no system key", agent: &AgentData{}, wantAurora: false},
		{name: "no agent", agent: nil, wantAurora: false},
		{name: "prefix only is not aurora", agent: &AgentData{SystemKey: "aurorafoo"}, wantAurora: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			task := Task{Agent: tc.agent}
			if got := isAuroraTask(task); got != tc.wantAurora {
				t.Fatalf("isAuroraTask() = %v, want %v", got, tc.wantAurora)
			}
		})
	}
}

func TestAuroraToolSurface(t *testing.T) {
	t.Parallel()

	mode, tools := auroraToolSurface("claude")
	if mode != "default" {
		t.Fatalf("claude permission mode = %q, want %q", mode, "default")
	}
	for _, want := range []string{"Bash", "WebFetch", "WebSearch"} {
		if !slices.Contains(tools, want) {
			t.Fatalf("claude disallowed tools %v missing %q", tools, want)
		}
	}

	// Providers not yet onboarded onto the sandbox image keep no narrowing; the
	// sandbox image build (external pending capability) must not install them.
	mode, tools = auroraToolSurface("codex")
	if mode != "" || tools != nil {
		t.Fatalf("codex surface = (%q, %v), want empty (un-narrowed)", mode, tools)
	}
}
