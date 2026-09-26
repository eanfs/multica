package aurorafleet

import "testing"

func TestDockerStateToNodeState(t *testing.T) {
	cases := map[string]string{
		"created":    StateStarting,
		"running":    StateOnline,
		"restarting": StateStarting,
		"removing":   StateDraining,
		"exited":     StateStopped,
		"paused":     StateStopped,
		"dead":       StateFailed,
		"":           StateStopped,
	}
	for in, want := range cases {
		if got := dockerStateToNodeState(in); got != want {
			t.Errorf("dockerStateToNodeState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePSLine(t *testing.T) {
	state, ok := parsePSLine("abc123\tworker-1\taurora-sandbox:latest\trunning")
	if !ok || state != StateOnline {
		t.Fatalf("parsePSLine = %q, %v", state, ok)
	}
	for _, blank := range []string{"", "  \n"} {
		if _, ok := parsePSLine(blank); ok {
			t.Fatalf("parsePSLine(%q) returned ok for a blank row", blank)
		}
	}
}
