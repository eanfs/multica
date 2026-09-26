package aurorafleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	policyTestWorkspaceID = "11111111-1111-4111-8111-111111111111"
	policyTestNodeID      = "22222222-2222-4222-8222-222222222222"
	policyTestRuntimeID   = "33333333-3333-4333-8333-333333333333"
	policyTestDaemonID    = "44444444-4444-4444-8444-444444444444"
)

// validTestPolicy returns a policy whose external references (images, seccomp
// path, origin, secret root) satisfy validation, using t's temp dir.
func validTestPolicy(t *testing.T) Policy {
	t.Helper()
	return Policy{
		SandboxImage: "ghcr.io/eanfs/multica-aurora-sandbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ProxyImage:   "ghcr.io/eanfs/multica-aurora-egress@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		SeccompPath:  "/etc/multica/aurora/seccomp.json",
		ServerOrigin: "https://multica.example.com",
		SecretRoot:   t.TempDir(),
	}
}

func testSpec(secretRoot string) WorkspaceNodeSpec {
	return WorkspaceNodeSpec{
		NodeID:         policyTestNodeID,
		WorkspaceID:    policyTestWorkspaceID,
		RuntimeID:      policyTestRuntimeID,
		DaemonID:       policyTestDaemonID,
		EnrollmentFile: filepath.Join(secretRoot, testNodeID, "enrollment"),
	}
}

func TestSandboxArgsEnforceImmutablePolicy(t *testing.T) {
	p := validTestPolicy(t)
	spec := testSpec(p.SecretRoot)
	if err := os.MkdirAll(filepath.Dir(spec.EnrollmentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.EnrollmentFile, []byte("mse_test"), 0o400); err != nil {
		t.Fatal(err)
	}

	args, err := p.SandboxArgs(spec)
	if err != nil {
		t.Fatalf("SandboxArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	required := [][]string{
		{"run", "--detach", "--pull", "never", "--init", "--restart", "no", "--stop-timeout", "30"},
		{"--user", "10001:10001", "--read-only", "--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges:true"},
		{"--security-opt", "seccomp=" + p.SeccompPath},
		{"--security-opt", "apparmor=multica-aurora-sandbox"},
		{"--pids-limit", "256", "--memory", "4g", "--memory-swap", "4g", "--cpus", "2"},
		{"--ulimit", "nofile=1024:1024"},
		{"--tmpfs", "/workspace:rw,nosuid,nodev,noexec,size=2147483648,uid=10001,gid=10001,mode=0700"},
		{"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=268435456,uid=10001,gid=10001,mode=0700"},
		{"--tmpfs", "/run:rw,nosuid,nodev,noexec,size=16777216,uid=10001,gid=10001,mode=0755"},
		{"--mount", "type=bind,src=" + spec.EnrollmentFile + ",dst=/run/secrets/aurora-enrollment,readonly"},
		{"-e", "MULTICA_SERVER_URL=" + p.ServerOrigin},
		{"-e", "MULTICA_MANAGED=1"},
		{"-e", "MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE=/run/secrets/aurora-enrollment"},
		{"-e", "MULTICA_AGENT_TIMEOUT=30m"},
		{"-e", "HTTP_PROXY=http://egress:3128"},
		{"-e", "HTTPS_PROXY=http://egress:3128"},
		{"-e", "NO_PROXY=egress,127.0.0.1,localhost"},
		{"--name"},
		{p.SandboxImage},
	}
	for _, want := range required {
		if !containsSubslice(args, want) {
			t.Errorf("sandbox args missing %v: %s", want, joined)
		}
	}
	for _, forbidden := range []string{"--privileged", "--cap-add", "--pid=host", "--network=host", "/var/run/docker.sock", "--device"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("sandbox args contain forbidden %q: %s", forbidden, joined)
		}
	}
	// The image must be the last argument: nothing can follow it.
	if args[len(args)-1] != p.SandboxImage {
		t.Errorf("image is not the final argument: %s", joined)
	}
	// The only network flag is the per-workspace internal network name.
	for i, a := range args {
		if a == "--network" && !strings.HasPrefix(args[i+1], "aurora-ws-") {
			t.Errorf("--network is not a workspace internal network: %s", args[i+1])
		}
	}
	// Labels are the controlled com.multica.aurora.* namespace only.
	for i, a := range args {
		if a == "--name" && !strings.HasPrefix(args[i+1], "aurora-sbx-") {
			t.Errorf("sandbox container name is not policy-derived: %s", args[i+1])
		}
		if a == "--label" && !strings.HasPrefix(args[i+1], "com.multica.aurora.") {
			t.Errorf("label outside the controlled namespace: %s", args[i+1])
		}
	}
}

func TestProxyArgsEnforceSidecarPolicy(t *testing.T) {
	p := validTestPolicy(t)
	args, err := p.ProxyArgs("aurora-egr-test")
	if err != nil {
		t.Fatalf("ProxyArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	required := [][]string{
		{"run", "--detach", "--pull", "never", "--init", "--restart", "no"},
		{"--user", "10001:10001", "--read-only", "--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges:true"},
		{"--pids-limit", "64", "--memory", "256m", "--memory-swap", "256m", "--cpus", "0.25"},
		{"--ulimit", "nofile=512:512"},
		{"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=33554432,uid=10001,gid=10001,mode=0700"},
	}
	for _, want := range required {
		if !containsSubslice(args, want) {
			t.Errorf("proxy args missing %v: %s", want, joined)
		}
	}
	for _, forbidden := range []string{"--privileged", "--cap-add", "--mount", "enrollment", "mse_", "--device"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("proxy args contain forbidden %q: %s", forbidden, joined)
		}
	}
	// Non-secret configuration only: the exact server origin and the allowed
	// host list (compiled defaults plus operator extras).
	if !containsSubslice(args, []string{"-e", "MULTICA_EGRESS_SERVER_ORIGIN=" + p.ServerOrigin}) {
		t.Errorf("proxy args missing server origin env: %s", joined)
	}
	wantHosts := strings.Join(append([]string{
		"api.anthropic.com:443",
		"ark.cn-beijing.volces.com:443",
		"api.openai.com:443",
		"openspeech.bytedance.com:443",
	}, p.ExtraEgressHosts...), ",")
	if !containsSubslice(args, []string{"-e", "MULTICA_EGRESS_ALLOWED_HOSTS=" + wantHosts}) {
		t.Errorf("proxy args missing allowed host list: %s", joined)
	}
	// The proxy joins the fleet uplink network at creation; the workspace
	// internal network is attached afterwards so "egress" resolves inside it.
	if !containsSubslice(args, []string{"--network", p.UplinkNetwork()}) {
		t.Errorf("proxy args missing uplink network: %s", joined)
	}
}

func TestEgressNetworkConnectJoinsWorkspaceNetwork(t *testing.T) {
	p := validTestPolicy(t)
	args := p.EgressNetworkConnect("aurora-egr-deadbeefdeadbeef", "aurora-ws-deadbeefdeadbeef")
	if !containsSubslice(args, []string{"network", "connect", "--alias", "egress", "aurora-ws-deadbeefdeadbeef", "aurora-egr-deadbeefdeadbeef"}) {
		t.Errorf("unexpected connect args: %v", args)
	}
}

func TestImageDigestRequired(t *testing.T) {
	cases := map[string]bool{
		"ghcr.io/eanfs/img@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": true,
		"alpine@sha256:" + strings.Repeat("a", 64):                                                  true,
		"ghcr.io/eanfs/img:latest":                                                                  false, // tag only
		"ghcr.io/eanfs/img":                                                                         false, // no digest
		"ghcr.io/eanfs/img@sha256:0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF": false, // uppercase
		"ghcr.io/eanfs/img@sha256:0123456789abcdef":                                                 false, // truncated
		"ghcr.io/eanfs/img@md5:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef":    false, // wrong algorithm
		"ghcr.io/eanfs/img@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeX": false, // non-hex
		"ghcr.io/eanfs/img extra@sha256:" + strings.Repeat("a", 64):                                 false, // whitespace smuggle
	}
	for image, want := range cases {
		if err := validateDigestPinnedImage(image); (err == nil) != want {
			t.Errorf("validateDigestPinnedImage(%q) error = %v, want valid=%v", image, err, want)
		}
	}
}

func TestPolicyValidateRejectsIncompleteConfiguration(t *testing.T) {
	p := validTestPolicy(t)
	p.SandboxImage = "ghcr.io/eanfs/img:latest"
	if err := p.Validate(); err == nil {
		t.Error("expected tag-only sandbox image to fail validation")
	}

	p2 := validTestPolicy(t)
	p2.SeccompPath = "relative/seccomp.json"
	if err := p2.Validate(); err == nil {
		t.Error("expected relative seccomp path to fail validation")
	}

	p3 := validTestPolicy(t)
	p3.ServerOrigin = "not a url"
	if err := p3.Validate(); err == nil {
		t.Error("expected malformed server origin to fail validation")
	}

	p4 := validTestPolicy(t)
	p4.SecretRoot = "relative"
	if err := p4.Validate(); err == nil {
		t.Error("expected relative secret root to fail validation")
	}
}

func TestSecretMountRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	p := validTestPolicy(t)
	p.SecretRoot = root
	nodeDir := filepath.Join(root, testNodeID)
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "enrollment"), []byte("mse_test"), 0o400); err != nil {
		t.Fatal(err)
	}

	// A valid in-root regular file mounts read-only.
	args, err := p.secretMountArgs(filepath.Join(nodeDir, "enrollment"))
	if err != nil {
		t.Fatalf("secretMountArgs(valid): %v", err)
	}
	if !containsSubslice(args, []string{"--mount", "type=bind,src=" + filepath.Join(nodeDir, "enrollment") + ",dst=/run/secrets/aurora-enrollment,readonly"}) {
		t.Errorf("unexpected mount args: %v", args)
	}

	outside := filepath.Join(os.TempDir(), "multica-policy-test-outside-secret")
	if err := os.WriteFile(outside, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	cases := map[string]string{
		"relative path":       filepath.Join(testNodeID, "enrollment"),
		"outside secret root": outside,
		"parent escape":       filepath.Join(root, testNodeID, "..", "..", "etc", "passwd"),
		"missing file":        filepath.Join(nodeDir, "does-not-exist"),
	}
	for name, path := range cases {
		if _, err := p.secretMountArgs(path); err == nil {
			t.Errorf("secretMountArgs(%s) accepted an unsafe path", name)
		}
	}

	// Create the symlink after the non-symlink negative cases above ran.
	if err := os.Symlink(filepath.Join(nodeDir, "enrollment"), filepath.Join(nodeDir, "enrollment-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.secretMountArgs(filepath.Join(nodeDir, "enrollment-link")); err == nil {
		t.Error("secretMountArgs accepted a symlinked secret")
	}
}

func TestLabelsRejectDuplicateKeys(t *testing.T) {
	labels := map[string]string{}
	if err := mergeLabels(labels, "com.multica.aurora.node", "a", "com.multica.aurora.node", "b"); err == nil {
		t.Fatal("mergeLabels accepted a duplicate label key")
	}
	if err := mergeLabels(labels, "com.multica.aurora.node", "a", "other.key", "b"); err == nil {
		t.Fatal("mergeLabels accepted a key outside the controlled namespace")
	}
}

func TestSpecIdentityRejectsDockerFlagInjection(t *testing.T) {
	p := validTestPolicy(t)
	spec := testSpec(p.SecretRoot)
	spec.WorkspaceID = "--privileged"
	if _, _, _, err := p.NodeNames(spec); err == nil {
		t.Error("NodeNames accepted a workspace ID containing a Docker flag")
	}
	spec2 := testSpec(p.SecretRoot)
	spec2.NodeID = "x --cap-add=SYS_ADMIN"
	if _, _, _, err := p.NodeNames(spec2); err == nil {
		t.Error("NodeNames accepted a node ID containing a Docker flag")
	}
}

func TestNodeNamesAreDeterministicAndSafe(t *testing.T) {
	p := validTestPolicy(t)
	spec := testSpec(p.SecretRoot)
	net1, proxy1, sandbox1, err := p.NodeNames(spec)
	if err != nil {
		t.Fatalf("NodeNames: %v", err)
	}
	net2, proxy2, sandbox2, err := p.NodeNames(spec)
	if err != nil {
		t.Fatalf("NodeNames: %v", err)
	}
	if net1 != net2 || proxy1 != proxy2 || sandbox1 != sandbox2 {
		t.Fatal("NodeNames is not deterministic for the same identity")
	}
	if net1 == proxy1 || proxy1 == sandbox1 {
		t.Fatal("network, proxy, and sandbox names must be distinct")
	}
	for _, name := range []string{net1, proxy1, sandbox1} {
		if len(name) > 64 || !safeNamePattern.MatchString(name) {
			t.Errorf("unsafe container/network name %q", name)
		}
	}
	// A different workspace must not collide.
	spec.WorkspaceID = "55555555-5555-4555-8555-555555555555"
	net3, _, _, err := p.NodeNames(spec)
	if err != nil {
		t.Fatalf("NodeNames: %v", err)
	}
	if net3 == net1 {
		t.Fatal("different workspaces produced the same network name")
	}
}

func TestWorkspaceNetworkCreateIsInternal(t *testing.T) {
	p := validTestPolicy(t)
	args := p.WorkspaceNetworkCreateArgs("aurora-ws-deadbeefdeadbeef")
	if !containsSubslice(args, []string{"network", "create", "--internal"}) {
		t.Errorf("workspace network is not internal: %v", args)
	}
	if !containsSubslice(args, []string{"--label", fleetLabel}) {
		t.Errorf("workspace network missing fleet label: %v", args)
	}
}

// containsSubslice reports whether want appears as a contiguous subslice of args.
func containsSubslice(args []string, want []string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
