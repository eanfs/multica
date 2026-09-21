package aurorafleet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerStatusToNodeStatus(t *testing.T) {
	cases := map[string]Status{
		"Up 5 seconds":           StatusRunning,
		"Restarting (1) 1s ago":  StatusRebooting,
		"Created":                StatusProvisioning,
		"Exited (0) 2 hours ago": StatusStopped,
		"Dead":                   StatusError,
		"":                       StatusStopped,
	}
	for in, want := range cases {
		if got := dockerStatusToNodeStatus(in); got != want {
			t.Errorf("dockerStatusToNodeStatus(%q) = %q, want %q", in, got, want)
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

	backend = NewDockerBackend("aurora-sandbox:latest", map[string]string{
		EnvServerURL:    "http://multica.test",
		EnvSandboxToken: "secret-token",
	})
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
// label, image, sorted env, and optional name all reach the docker CLI.
func TestDockerBackendCreateBuildsRunArgs(t *testing.T) {
	backend, argLog := fakeDocker(t, "abc123",
		"abc123\tworker-1\taurora-sandbox:latest\tUp 5 seconds\t2026-01-01T00:00:00Z")

	node, err := backend.Create(context.Background(), CreateRequest{
		Name:  "worker-1",
		Image: "custom:image",
		Env:   map[string]string{"EXTRA": "1"},
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
		"-e AURORA_SANDBOX_TOKEN=secret-token",
		"-e EXTRA=1",
		"-e MULTICA_SERVER_URL=http://multica.test",
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
