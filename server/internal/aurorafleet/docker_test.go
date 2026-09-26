package aurorafleet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerStateToNodeStatus(t *testing.T) {
	cases := map[string]Status{
		"created":    StatusProvisioning,
		"running":    StatusRunning,
		"restarting": StatusRebooting,
		"removing":   StatusTerminating,
		"exited":     StatusStopped,
		"paused":     StatusStopped,
		"dead":       StatusError,
		"":           StatusStopped,
	}
	for in, want := range cases {
		if got := dockerStateToNodeStatus(in); got != want {
			t.Errorf("dockerStateToNodeStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePSLine(t *testing.T) {
	node, ok := parsePSLine("abc123\tworker-1\taurora-sandbox:latest\trunning\t2026-01-01T00:00:00Z")
	if !ok {
		t.Fatal("parsePSLine rejected a valid row")
	}
	if node.ID != "abc123" || node.Name != "worker-1" || node.Status != StatusRunning {
		t.Fatalf("parsed node = %+v", node)
	}
	if node.CreatedAt.IsZero() {
		t.Fatal("parsePSLine did not parse CreatedAt")
	}
	for _, blank := range []string{"", "  \n"} {
		if _, ok := parsePSLine(blank); ok {
			t.Fatalf("parsePSLine(%q) returned ok for a blank row", blank)
		}
	}
}

// fakeDocker writes a shell script that records its args and returns canned
// output, then points a DockerBackend at it. Returned is a func that reads the
// recorded arg log.
func fakeDocker(t *testing.T, runOutput, psOutput string) (backend *DockerBackend, argLog func() string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	script := `#!/bin/sh
echo "$@" >> "` + logPath + `"
case "$1" in
  run) printf '%s\n' "` + runOutput + `" ;;
  ps) printf '%s\n' "` + psOutput + `" ;;
  info) exit 0 ;;
  *) exit 0 ;;
esac
`
	scriptPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	backend = NewDockerBackend()
	backend.dockerPath = scriptPath

	argLog = func() string {
		b, err := os.ReadFile(logPath)
		if err != nil {
			return ""
		}
		return string(b)
	}
	return backend, argLog
}

// TestDockerBackendCreateBuildsRunArgs pins the container bootstrap: the fleet
// label, image, sorted env/labels, and optional name all reach the docker CLI.
func TestDockerBackendCreateBuildsRunArgs(t *testing.T) {
	backend, argLog := fakeDocker(t, "abc123", "")

	node, err := backend.Create(context.Background(), CreateRequest{
		Name:   "worker-1",
		Image:  "custom:image",
		Env:    map[string]string{"EXTRA": "1"},
		Labels: map[string]string{"tier": "gpu"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if node.ID != "abc123" || node.Status != StatusRunning {
		t.Fatalf("created node = %+v, want id=abc123 running", node)
	}

	log := argLog()
	for _, want := range []string{
		"run -d --label multica-aurora-node=1 --name worker-1",
		"-e EXTRA=1",
		"--label tier=gpu",
		"custom:image",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("docker args missing %q; got:\n%s", want, log)
		}
	}
}

func TestDockerBackendStatusNotFound(t *testing.T) {
	backend, _ := fakeDocker(t, "abc123", "")
	if _, err := backend.Status(context.Background(), "missing"); err != ErrNotFound {
		t.Fatalf("status err = %v, want ErrNotFound", err)
	}
}
