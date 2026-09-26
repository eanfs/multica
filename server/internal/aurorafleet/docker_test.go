package aurorafleet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// Backend-level policy construction tests: EnsureWorkspaceNode must build
// every docker invocation from the immutable Policy.

// scriptedDocker installs a fake docker executable that records every
// invocation into a file and answers inspect with a running container.
func scriptedDocker(t *testing.T) (dockerPath, record string) {
	t.Helper()
	dir := t.TempDir()
	record = dir + "/record"
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> \"$DOCKER_RECORD\"\n" +
		"if [ \"$1\" = \"inspect\" ]; then echo true; else echo fakecontainerid; fi\n"
	path := dir + "/docker"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_RECORD", record)
	return path, record
}

func policyBackendForTest(t *testing.T) (*DockerBackend, string) {
	t.Helper()
	p := validTestPolicy(t)
	dockerPath, record := scriptedDocker(t)
	b := NewDockerBackendWithPolicy(p)
	b.dockerPath = dockerPath
	return b, record
}

func TestEnsureWorkspaceNodeFailsClosedWithoutPolicy(t *testing.T) {
	dockerPath, _ := scriptedDocker(t)
	b := NewDockerBackend()
	b.dockerPath = dockerPath
	if _, err := b.EnsureWorkspaceNode(context.Background(), testSpec(t.TempDir())); err == nil {
		t.Fatal("EnsureWorkspaceNode succeeded without a configured policy")
	}
}

func TestEnsureWorkspaceNodeBuildsFromPolicy(t *testing.T) {
	b, record := policyBackendForTest(t)
	spec := testSpec(b.policy.SecretRoot)
	if err := os.MkdirAll(filepath.Dir(spec.EnrollmentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.EnrollmentFile, []byte("mse_test"), 0o400); err != nil {
		t.Fatal(err)
	}

	node, err := b.EnsureWorkspaceNode(context.Background(), spec)
	if err != nil {
		t.Fatalf("EnsureWorkspaceNode: %v", err)
	}
	if node.ID == "" || node.ProxyID == "" || node.NetworkID == "" {
		t.Fatalf("incomplete node returned: %+v", node)
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	invocations := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(invocations) < 4 {
		t.Fatalf("expected at least 4 docker invocations, got %d: %v", len(invocations), invocations)
	}
	joined := strings.Join(invocations, "\n")
	if !strings.Contains(invocations[0], "network create --internal") {
		t.Errorf("first invocation is not the internal network create: %q", invocations[0])
	}
	// The egress sidecar is created before the sandbox and connected to the
	// workspace network under the egress alias.
	proxyIdx, sandboxIdx := -1, -1
	for i, inv := range invocations {
		if strings.HasPrefix(inv, "run") && strings.Contains(inv, "multica-aurora-egress@") {
			proxyIdx = i
		}
		if strings.HasPrefix(inv, "run") && strings.Contains(inv, "multica-aurora-sandbox@") {
			sandboxIdx = i
		}
	}
	if proxyIdx == -1 || sandboxIdx == -1 {
		t.Fatalf("missing proxy or sandbox run invocation: %v", invocations)
	}
	if proxyIdx > sandboxIdx {
		t.Errorf("proxy was created after the sandbox: %v", invocations)
	}
	if !strings.Contains(joined, "network connect --alias egress") {
		t.Errorf("egress sidecar was not connected with the egress alias: %s", joined)
	}
	// The sandbox run carries the hardened flags.
	if !strings.Contains(invocations[sandboxIdx], "--cap-drop ALL") ||
		!strings.Contains(invocations[sandboxIdx], "--read-only") ||
		!strings.Contains(invocations[sandboxIdx], "--user 10001:10001") ||
		!strings.Contains(invocations[sandboxIdx], "--security-opt seccomp=") {
		t.Errorf("sandbox invocation missing hardening: %q", invocations[sandboxIdx])
	}
}

func TestEnsureWorkspaceNodeRollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	record := dir + "/record"
	// A fake docker whose network create succeeds but run fails.
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> \"$DOCKER_RECORD\"\n" +
		"if [ \"$1\" = \"run\" ]; then echo boom >&2; exit 1; fi\n" +
		"if [ \"$1\" = \"inspect\" ]; then echo true; else echo fakecontainerid; fi\n"
	path := dir + "/docker"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_RECORD", record)

	p := validTestPolicy(t)
	b := NewDockerBackendWithPolicy(p)
	b.dockerPath = path
	spec := testSpec(p.SecretRoot)
	if err := os.MkdirAll(filepath.Dir(spec.EnrollmentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.EnrollmentFile, []byte("mse_test"), 0o400); err != nil {
		t.Fatal(err)
	}

	if _, err := b.EnsureWorkspaceNode(context.Background(), spec); err == nil {
		t.Fatal("expected ensure to fail when docker run fails")
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "rm -f aurora-egr-") || !strings.Contains(string(data), "network rm aurora-net-") {
		t.Errorf("rollback did not remove the created resources:\n%s", data)
	}
}
