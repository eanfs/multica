package daemon

import (
	"context"
	"errors"
	"log/slog"
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

	surface, ok := auroraToolSurface("claude")
	if !ok {
		t.Fatal("claude must have a reviewed surface")
	}
	if surface.permissionMode != "default" {
		t.Fatalf("claude permission mode = %q, want %q", surface.permissionMode, "default")
	}
	for _, want := range []string{"Bash", "WebFetch", "WebSearch"} {
		if !slices.Contains(surface.disallowed, want) {
			t.Fatalf("claude disallowed tools %v missing %q", surface.disallowed, want)
		}
	}

	// Un-onboarded providers fail closed so runTask refuses the task instead of
	// falling back to the default autonomous (bypass) surface.
	if _, ok := auroraToolSurface("codex"); ok {
		t.Fatal("codex must not yet have a reviewed surface")
	}
}

// Mirrors TestRunTaskRejectsMismatchedAgentIdentityBeforePreparation: the
// fail-closed gate sits before workdir preparation, so a bare Daemon plus a
// matching identity is enough to reach it.
func TestRunTaskRejectsAuroraWithoutReviewedSurface(t *testing.T) {
	t.Parallel()

	d := &Daemon{}
	_, err := d.runTask(context.Background(), Task{
		ID:          "task-aurora-codex",
		WorkspaceID: "workspace-a",
		AgentID:     "agent-a",
		Agent:       &AgentData{ID: "agent-a", SystemKey: "aurora:image"},
	}, "codex", 0, slog.Default())
	if !errors.Is(err, errAuroraSurfaceNotOnboarded) {
		t.Fatalf("runTask error = %v, want aurora surface not onboarded", err)
	}
}
