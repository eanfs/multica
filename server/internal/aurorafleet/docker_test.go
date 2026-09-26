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
	if !strings.Contains(string(data), "rm -f aurora-egr-") || !strings.Contains(string(data), "network rm aurora-ws-") {
		t.Errorf("rollback did not remove the created resources:\n%s", data)
	}
}

// TestDockerWorkspaceNetworkIsInternalAndUplinkIsSeparate covers the Step-6
// wiring: an internal per-workspace network, the egress sidecar aliased as
// "egress" on it, and the sidecar's separate pre-created uplink bridge.
func TestDockerWorkspaceNetworkIsInternalAndUplinkIsSeparate(t *testing.T) {
	b, record := policyBackendForTest(t)
	spec := testSpec(b.policy.SecretRoot)
	if err := os.MkdirAll(filepath.Dir(spec.EnrollmentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.EnrollmentFile, []byte("mse_test"), 0o400); err != nil {
		t.Fatal(err)
	}
	network, _, _, err := b.policy.NodeNames(spec)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := b.EnsureWorkspaceNode(context.Background(), spec); err != nil {
		t.Fatalf("EnsureWorkspaceNode: %v", err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	invocations := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(invocations) == 0 || !strings.HasPrefix(invocations[0], "network create --internal") || !strings.Contains(invocations[0], network) {
		t.Fatalf("first invocation is not the internal workspace network create: %v", invocations)
	}
	if !strings.HasPrefix(network, "aurora-ws-") {
		t.Errorf("workspace network %q is not named aurora-ws-<hash>", network)
	}

	var proxyInv, sandboxInv string
	for _, inv := range invocations {
		switch {
		case strings.HasPrefix(inv, "run") && strings.Contains(inv, "aurora-egr-"):
			proxyInv = inv
		case strings.HasPrefix(inv, "run") && strings.Contains(inv, "aurora-sbx-"):
			sandboxInv = inv
		}
	}
	if proxyInv == "" || sandboxInv == "" {
		t.Fatalf("missing proxy or sandbox run: %v", invocations)
	}
	if b.policy.UplinkNetwork() != "aurora-egress-uplink" {
		t.Errorf("uplink network = %q, want aurora-egress-uplink", b.policy.UplinkNetwork())
	}
	if !containsSubslice(strings.Fields(proxyInv), []string{"--network", b.policy.UplinkNetwork()}) {
		t.Errorf("egress sidecar is not on the pre-created uplink bridge: %q", proxyInv)
	}
	joined := strings.Join(invocations, "\n")
	if !strings.Contains(joined, "network connect --alias egress "+network+" ") {
		t.Errorf("egress sidecar was not attached with the egress alias: %s", joined)
	}
	// The sandbox is attached only to the internal network, never the uplink.
	if !strings.Contains(sandboxInv, "--network "+network) {
		t.Errorf("sandbox is not on the workspace internal network: %q", sandboxInv)
	}
	if strings.Contains(sandboxInv, b.policy.UplinkNetwork()) {
		t.Errorf("sandbox must never join the uplink network: %q", sandboxInv)
	}
	networks := 0
	for _, field := range strings.Fields(sandboxInv) {
		if field == "--network" {
			networks++
		}
	}
	if networks != 1 {
		t.Errorf("sandbox run declares %d networks, want exactly 1: %q", networks, sandboxInv)
	}
}

// TestEnsureWorkspaceNodeRollbackRemovesSecret asserts a partial failure cleans
// up the sandbox, proxy, network, and the controller-staged enrollment secret.
func TestEnsureWorkspaceNodeRollbackRemovesSecret(t *testing.T) {
	dir := t.TempDir()
	record := dir + "/record"
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
	if _, err := os.Stat(spec.EnrollmentFile); !os.IsNotExist(err) {
		t.Errorf("rollback left the staged enrollment secret in place (stat err = %v)", err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(data)
	for _, want := range []string{"rm -f aurora-egr-", "rm -f aurora-sbx-", "network rm aurora-ws-"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rollback did not run %q:\n%s", want, joined)
		}
	}
}

// TestSiblingNodeNamesDerivesPolicySiblings covers the name derivation the
// delete path depends on.
func TestSiblingNodeNamesDerivesPolicySiblings(t *testing.T) {
	proxy, network, ok := siblingNodeNames("aurora-sbx-0123456789abcdef")
	if !ok || proxy != "aurora-egr-0123456789abcdef" || network != "aurora-ws-0123456789abcdef" {
		t.Fatalf("siblingNodeNames = %q, %q, %v", proxy, network, ok)
	}
	for _, name := range []string{"", "aurora-sbx-", "some-other-container", "uuid"} {
		if _, _, ok := siblingNodeNames(name); ok {
			t.Errorf("siblingNodeNames(%q) accepted a non-sandbox name", name)
		}
	}
}

// deleteTestBackend builds a DockerBackend whose fake docker fails a direct
// removal of the node UUID, answers the controlled-label lookup with sandbox,
// and otherwise succeeds.
func deleteTestBackend(t *testing.T, sandbox string) (*DockerBackend, string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> \"$DOCKER_RECORD\"\n" +
		"if [ \"$1\" = \"rm\" ] && [ \"$3\" = \"" + policyTestNodeID + "\" ]; then echo 'Error: No such container' >&2; exit 1; fi\n" +
		"if [ \"$1\" = \"ps\" ]; then echo " + sandbox + "; exit 0; fi\n" +
		"exit 0\n"
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_RECORD", record)
	b := NewDockerBackendWithPolicy(validTestPolicy(t))
	b.dockerPath = path
	return b, record
}

// TestDeleteWorkspaceNodeByNameRemovesNodeProxyAndNetwork asserts the delete
// contract for the policy-derived backend node id the fleet reports.
func TestDeleteWorkspaceNodeByNameRemovesNodeProxyAndNetwork(t *testing.T) {
	b, record := deleteTestBackend(t, "aurora-sbx-cafecafecafecafe")
	if err := b.DeleteWorkspaceNode(context.Background(), "aurora-sbx-cafecafecafecafe"); err != nil {
		t.Fatalf("DeleteWorkspaceNode: %v", err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"rm -f aurora-sbx-cafecafecafecafe",
		"rm -f aurora-egr-cafecafecafecafe",
		"network rm aurora-ws-cafecafecafecafe",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("delete did not run %q:\n%s", want, data)
		}
	}
}

// TestDeleteWorkspaceNodeResolvesUUIDThroughLabel asserts a node UUID is
// resolved through the controlled label before removal.
func TestDeleteWorkspaceNodeResolvesUUIDThroughLabel(t *testing.T) {
	b, record := deleteTestBackend(t, "aurora-sbx-0123456789abcdef")
	if err := b.DeleteWorkspaceNode(context.Background(), policyTestNodeID); err != nil {
		t.Fatalf("DeleteWorkspaceNode: %v", err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(data)
	if !strings.Contains(joined, "--filter label=com.multica.aurora.node="+policyTestNodeID) {
		t.Errorf("delete did not look the node up by its controlled label:\n%s", joined)
	}
	for _, want := range []string{
		"rm -f aurora-sbx-0123456789abcdef",
		"rm -f aurora-egr-0123456789abcdef",
		"network rm aurora-ws-0123456789abcdef",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("delete did not run %q:\n%s", want, joined)
		}
	}
}
