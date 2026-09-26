//go:build auroradocker

// This file is the Linux-only Docker security acceptance for Aurora workspace
// sandbox nodes. It is compiled only under the auroradocker build tag and runs
// only on a Linux Docker Engine host; Docker Desktop on macOS is a functional
// smoke platform and cannot satisfy the AppArmor, cgroup, or kernel gates.
//
// It requires AURORA_RUN_DOCKER_SECURITY_TEST=1 before any Docker lookup, plus
// AURORA_SANDBOX_IMAGE, AURORA_PROXY_IMAGE, and AURORA_SECCOMP_PROFILE from
// deploy/aurora-sandbox/docker-security-test.sh.
package aurorafleet

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	dockerSecurityGateEnv    = "AURORA_RUN_DOCKER_SECURITY_TEST"
	dockerSecurityImageEnv   = "AURORA_SANDBOX_IMAGE"
	dockerSecurityProxyEnv   = "AURORA_PROXY_IMAGE"
	dockerSecuritySeccompEnv = "AURORA_SECCOMP_PROFILE"
	probeBinaryPath          = "/opt/aurora/bin/aurora-sandbox-probe"
)

// dockerSecurityConfig is the fixture configuration an operator supplies.
type dockerSecurityConfig struct {
	sandboxImage string
	proxyImage   string
	seccompPath  string
	secretRoot   string
}

// TestDockerSandboxLinuxSecurityBoundary provisions a real workspace node with
// the hardened policy, asserts every inspected container field, runs the fixed
// probe binary against the filesystem, privilege, namespace, PID, tmpfs, and
// network boundaries, proves rollback after each failure point, and proves a
// successful delete leaves nothing behind.
func TestDockerSandboxLinuxSecurityBoundary(t *testing.T) {
	if os.Getenv(dockerSecurityGateEnv) != "1" {
		t.Skip("set " + dockerSecurityGateEnv + "=1 to run the Linux Docker security acceptance")
	}
	if runtime.GOOS != "linux" {
		t.Fatalf("the Docker security acceptance requires a Linux kernel (runtime.GOOS=%s); Docker Desktop on macOS is only a functional smoke", runtime.GOOS)
	}

	cfg := loadDockerSecurityConfig(t)
	if !filepath.IsAbs(cfg.seccompPath) {
		t.Fatalf("%s %q must be an absolute path", dockerSecuritySeccompEnv, cfg.seccompPath)
	}
	if _, err := os.Stat(cfg.seccompPath); err != nil {
		t.Fatalf("seccomp profile %q is not readable: %v", cfg.seccompPath, err)
	}
	for name, image := range map[string]string{dockerSecurityImageEnv: cfg.sandboxImage, dockerSecurityProxyEnv: cfg.proxyImage} {
		if err := validateDigestPinnedImage(image); err != nil {
			t.Fatalf("%s %q must be digest-pinned: %v", name, image, err)
		}
	}

	// Every Docker lookup happens after the two gates above.
	ctx := context.Background()
	if _, stderr, err := dockerAttempt(ctx, "info"); err != nil {
		t.Fatalf("the Docker daemon is not reachable: %v: %s", err, stderr)
	}

	uplink := uplinkNetworkName
	gateway, createdUplink := ensureUplinkNetwork(t, uplink)
	if createdUplink {
		t.Cleanup(func() { _, _, _ = dockerAttempt(context.Background(), "network", "rm", uplink) })
	}
	origin := startFakeOrigin(t, gateway)

	policy := Policy{
		SandboxImage: cfg.sandboxImage,
		ProxyImage:   cfg.proxyImage,
		SeccompPath:  cfg.seccompPath,
		ServerOrigin: origin,
		SecretRoot:   cfg.secretRoot,
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("fixture policy is invalid: %v", err)
	}

	backend := NewDockerBackendWithPolicy(policy)
	spec := newSecuritySpec(t, cfg.secretRoot)
	network, proxy, sandbox, err := policy.NodeNames(spec)
	if err != nil {
		t.Fatalf("NodeNames: %v", err)
	}
	secret := writeEnrollmentFile(t, spec.EnrollmentFile)
	t.Cleanup(func() {
		removeDockerResources(sandbox, proxy)
		_, _, _ = dockerAttempt(context.Background(), "network", "rm", network)
		_ = os.Remove(spec.EnrollmentFile)
	})

	node, err := backend.EnsureWorkspaceNode(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureWorkspaceNode: %v", err)
	}
	if node.ID != sandbox {
		t.Fatalf("node ID = %q, want %q", node.ID, sandbox)
	}

	t.Run("inspect", func(t *testing.T) {
		inspect, err := backend.InspectContainer(ctx, sandbox)
		if err != nil {
			t.Fatalf("InspectContainer: %v", err)
		}
		raw, err := backend.InspectRaw(ctx, sandbox)
		if err != nil {
			t.Fatalf("InspectRaw: %v", err)
		}
		proxyRaw, err := backend.InspectRaw(ctx, proxy)
		if err != nil {
			t.Fatalf("InspectRaw(proxy): %v", err)
		}
		assertSandboxBoundary(t, inspect, raw, spec, network, uplink, secret, cfg)
		assertNoSecret(t, secret, map[string]string{"proxy inspect": string(proxyRaw)})
		for name, image := range map[string]string{"sandbox": cfg.sandboxImage, "proxy": cfg.proxyImage} {
			history, err := backend.ImageHistory(ctx, image)
			if err != nil {
				t.Fatalf("ImageHistory(%s): %v", name, err)
			}
			assertNoSecret(t, secret, map[string]string{name + " image history": history})
		}
	})

	t.Run("adversarial", func(t *testing.T) {
		runAdversarialProbes(t, backend, sandbox, network, origin, cfg)
	})

	t.Run("rollback", func(t *testing.T) {
		runRollbackCases(t, policy, cfg)
	})

	t.Run("delete", func(t *testing.T) {
		runDeleteCase(t, policy, cfg)
	})
}

// loadDockerSecurityConfig reads the operator environment. It never contacts
// Docker, so a missing variable fails before any daemon lookup.
func loadDockerSecurityConfig(t *testing.T) dockerSecurityConfig {
	t.Helper()
	return dockerSecurityConfig{
		sandboxImage: requiredEnv(t, dockerSecurityImageEnv),
		proxyImage:   requiredEnv(t, dockerSecurityProxyEnv),
		seccompPath:  requiredEnv(t, dockerSecuritySeccompEnv),
		secretRoot:   t.TempDir(),
	}
}

func requiredEnv(t *testing.T, key string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		t.Fatalf("%s is required; deploy/aurora-sandbox/docker-security-test.sh supplies it", key)
	}
	return value
}

// assertSandboxBoundary asserts every Step-3 field of the inspected sandbox and
// scans the complete inspection for the staged enrollment secret.
func assertSandboxBoundary(t *testing.T, inspect ContainerInspect, raw []byte, spec WorkspaceNodeSpec, network, uplink, secret string, cfg dockerSecurityConfig) {
	t.Helper()
	if inspect.Config.User != sandboxUser {
		t.Errorf("Config.User = %q, want %q", inspect.Config.User, sandboxUser)
	}
	if !inspect.HostConfig.ReadonlyRootfs {
		t.Errorf("read-only root filesystem is not set")
	}
	if !containsString(inspect.HostConfig.CapDrop, "ALL") {
		t.Errorf("HostConfig.CapDrop = %v, want ALL", inspect.HostConfig.CapDrop)
	}
	if len(inspect.HostConfig.CapAdd) != 0 {
		t.Errorf("HostConfig.CapAdd = %v, want empty", inspect.HostConfig.CapAdd)
	}
	assertSecurityOptPrefix(t, inspect.HostConfig.SecurityOpt, "no-new-privileges")
	assertSeccompOpt(t, inspect.HostConfig.SecurityOpt, cfg.seccompPath)
	assertSecurityOptPrefix(t, inspect.HostConfig.SecurityOpt, "apparmor="+appArmorProfile)
	if inspect.HostConfig.Memory != 4294967296 {
		t.Errorf("HostConfig.Memory = %d, want 4294967296", inspect.HostConfig.Memory)
	}
	if inspect.HostConfig.MemorySwap != 4294967296 {
		t.Errorf("HostConfig.MemorySwap = %d, want 4294967296", inspect.HostConfig.MemorySwap)
	}
	if inspect.HostConfig.NanoCpus != 2000000000 {
		t.Errorf("HostConfig.NanoCpus = %d, want 2000000000", inspect.HostConfig.NanoCpus)
	}
	if inspect.HostConfig.PidsLimit == nil || *inspect.HostConfig.PidsLimit != 256 {
		t.Errorf("HostConfig.PidsLimit = %v, want 256", inspect.HostConfig.PidsLimit)
	}
	if inspect.HostConfig.Privileged {
		t.Errorf("container is privileged")
	}
	if len(inspect.HostConfig.Devices) != 0 {
		t.Errorf("container has host devices: %v", inspect.HostConfig.Devices)
	}
	for field, mode := range map[string]string{
		"PidMode":     inspect.HostConfig.PidMode,
		"IpcMode":     inspect.HostConfig.IpcMode,
		"UTSMode":     inspect.HostConfig.UTSMode,
		"UsernsMode":  inspect.HostConfig.UsernsMode,
		"NetworkMode": inspect.HostConfig.NetworkMode,
	} {
		if mode == "host" {
			t.Errorf("HostConfig.%s = host", field)
		}
	}
	if len(inspect.HostConfig.Binds) != 0 {
		t.Errorf("container has host binds: %v", inspect.HostConfig.Binds)
	}
	if len(inspect.Mounts) != 1 {
		t.Errorf("container mounts = %v, want exactly the read-only enrollment secret", inspect.Mounts)
	} else {
		mount := inspect.Mounts[0]
		if mount.Type != "bind" || mount.Destination != enrollmentSecretMountPath || mount.RW {
			t.Errorf("mount = %+v, want a read-only bind at %s", mount, enrollmentSecretMountPath)
		}
		if filepath.Clean(mount.Source) != filepath.Clean(spec.EnrollmentFile) {
			t.Errorf("mount source = %q, want the staged enrollment file %q", mount.Source, spec.EnrollmentFile)
		}
	}
	if len(inspect.NetworkSettings.Networks) != 1 {
		t.Errorf("container networks = %v, want only the workspace internal network", networkKeys(inspect.NetworkSettings.Networks))
	}
	if _, ok := inspect.NetworkSettings.Networks[network]; !ok {
		t.Errorf("container is not attached to the workspace network %q", network)
	}
	if internal := strings.TrimSpace(mustDocker(t, "network", "inspect", "--format", "{{.Internal}}", network)); internal != "true" {
		t.Errorf("workspace network %s Internal = %q, want true", network, internal)
	}
	if strings.Contains(string(raw), uplink) {
		t.Errorf("container inspect names the fleet uplink network %q", uplink)
	}
	if bytes.Contains(raw, []byte("docker.sock")) {
		t.Errorf("container inspect references a Docker socket")
	}
	if !strings.Contains(inspect.Config.Image, pinnedDigest(t, cfg.sandboxImage)) {
		t.Errorf("Config.Image = %q, want the digest-pinned reference %q", inspect.Config.Image, cfg.sandboxImage)
	}
	assertNoSecret(t, secret, map[string]string{
		"container inspect": string(raw),
		"environment":       strings.Join(inspect.Config.Env, "\n"),
		"labels":            fmt.Sprint(inspect.Config.Labels),
		"command":           strings.Join(append(append([]string{}, inspect.Config.Entrypoint...), inspect.Config.Cmd...), " "),
		"mounts":            fmt.Sprint(inspect.Mounts),
	})
}

// assertNoSecret fails when any evidence text contains the raw secret value.
func assertNoSecret(t *testing.T, secret string, evidence map[string]string) {
	t.Helper()
	for name, text := range evidence {
		if strings.Contains(text, secret) {
			t.Errorf("%s exposes the raw enrollment secret value", name)
		}
	}
}

// runAdversarialProbes drives the fixed probe binary inside the sandbox.
func runAdversarialProbes(t *testing.T, backend *DockerBackend, sandbox, network, origin string, cfg dockerSecurityConfig) {
	t.Helper()
	requireAllowed(t, runProbe(t, backend, sandbox, "fs-write-workspace"), "fs-write-workspace")
	requireBlocked(t, runProbe(t, backend, sandbox, "fs-write-root"), "fs-write-root")
	requireBlocked(t, runProbe(t, backend, sandbox, "privilege-escalate"), "privilege-escalate")
	requireBlocked(t, runProbe(t, backend, sandbox, "raw-socket"), "raw-socket")
	requireSeccompKilled(t, runProbe(t, backend, sandbox, "mount"), "mount")
	requireSeccompKilled(t, runProbe(t, backend, sandbox, "unshare"), "unshare")
	requireSeccompKilled(t, runProbe(t, backend, sandbox, "ptrace"), "ptrace")
	requireBlocked(t, runProbe(t, backend, sandbox, "fork-pressure", "256"), "fork-pressure")

	for _, quota := range []struct {
		path string
		size int64
	}{
		{"/workspace/quota", 2147483648},
		{"/tmp/quota", 268435456},
		{"/run/quota", 16777216},
	} {
		name := "tmpfs-quota " + quota.path
		requireBlocked(t, runProbe(t, backend, sandbox, "tmpfs-quota", quota.path, strconv.FormatInt(quota.size, 10)), name)
	}

	gateway := networkGateway(t, network)
	for _, target := range []string{
		"1.1.1.1:443",
		"10.0.0.1:443",
		"192.168.0.1:443",
		"169.254.169.254:80",
		net.JoinHostPort(gateway, "9"),
		"example.com:443",
	} {
		requireBlocked(t, runProbe(t, backend, sandbox, "dial", target), "dial "+target)
	}

	// The exact fake Multica origin is the sidecar's only plain-HTTP allowance.
	waitForAllowed(t, backend, sandbox, 20*time.Second, "proxy-get", origin+"/aurora-acceptance")
	originHost, originPort, err := net.SplitHostPort(strings.TrimPrefix(origin, "http://"))
	if err != nil {
		t.Fatalf("fake origin %q: %v", origin, err)
	}
	wrongPort := 8443
	if originPort == strconv.Itoa(wrongPort) {
		wrongPort = 8444
	}
	requireBlocked(t, runProbe(t, backend, sandbox, "proxy-get", "http://"+net.JoinHostPort(originHost, strconv.Itoa(wrongPort))+"/"), "proxy-get wrong port")
	requireBlocked(t, runProbe(t, backend, sandbox, "proxy-get", "http://unknown.invalid/"), "proxy-get unknown host")
	requireBlocked(t, runProbe(t, backend, sandbox, "proxy-connect", "https://api.anthropic.com:8443/"), "proxy-connect wrong port")
	requireBlocked(t, runProbe(t, backend, sandbox, "proxy-connect", "https://10.0.0.1:443/"), "proxy-connect private target")
	requireBlocked(t, runProbe(t, backend, sandbox, "proxy-connect", "https://unknown.invalid/"), "proxy-connect unknown host")

	// A second workspace network and listener must be invisible to this sandbox.
	otherNetwork := networkNamePrefix + randomHex(t, 8)
	mustDocker(t, "network", "create", "--label", fleetLabel, otherNetwork)
	t.Cleanup(func() { _, _, _ = dockerAttempt(context.Background(), "network", "rm", otherNetwork) })
	peerName := "aurora-peer-" + randomHex(t, 8)
	peerIP := startPeerContainer(t, cfg.sandboxImage, peerName, otherNetwork, "9000")
	requireBlocked(t, runProbe(t, backend, sandbox, "network-visibility", peerName, peerIP, "9000"), "network-visibility")
}

// runRollbackCases injects a Docker failure at each provisioning stage and
// requires that no container, network, or enrollment secret survives.
func runRollbackCases(t *testing.T, policy Policy, cfg dockerSecurityConfig) {
	t.Helper()
	realDocker := mustLookPath(t, "docker")
	cases := []struct {
		name           string
		match          string
		createThenFail bool
	}{
		{"after_network_creation", proxyNamePrefix, false},
		{"after_proxy_creation", sandboxNamePrefix, false},
		{"after_sandbox_creation", sandboxNamePrefix, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			spec := newSecuritySpec(t, cfg.secretRoot)
			network, proxy, sandbox, err := policy.NodeNames(spec)
			if err != nil {
				t.Fatalf("NodeNames: %v", err)
			}
			writeEnrollmentFile(t, spec.EnrollmentFile)
			t.Cleanup(func() {
				removeDockerResources(sandbox, proxy)
				_, _, _ = dockerAttempt(context.Background(), "network", "rm", network)
				_ = os.Remove(spec.EnrollmentFile)
			})
			shim := writeFailOnceShim(t, realDocker, tc.match, tc.createThenFail)
			failing := NewDockerBackendWithPolicy(policy)
			failing.dockerPath = shim
			if _, err := failing.EnsureWorkspaceNode(context.Background(), spec); err == nil {
				t.Fatalf("EnsureWorkspaceNode succeeded despite the injected failure %s", tc.name)
			}
			assertNodeAbsent(t, sandbox, proxy, network, spec.EnrollmentFile)
		})
	}
}

// runDeleteCase provisions a node, deletes it through the controller route, and
// requires the same empty result as a rollback.
func runDeleteCase(t *testing.T, policy Policy, cfg dockerSecurityConfig) {
	t.Helper()
	spec := newSecuritySpec(t, cfg.secretRoot)
	network, proxy, sandbox, err := policy.NodeNames(spec)
	if err != nil {
		t.Fatalf("NodeNames: %v", err)
	}
	writeEnrollmentFile(t, spec.EnrollmentFile)
	backend := NewDockerBackendWithPolicy(policy)
	t.Cleanup(func() {
		removeDockerResources(sandbox, proxy)
		_, _, _ = dockerAttempt(context.Background(), "network", "rm", network)
		_ = os.Remove(spec.EnrollmentFile)
	})
	if _, err := backend.EnsureWorkspaceNode(context.Background(), spec); err != nil {
		t.Fatalf("EnsureWorkspaceNode: %v", err)
	}
	if _, err := backend.InspectContainer(context.Background(), sandbox); err != nil {
		t.Fatalf("sandbox is not inspectable before delete: %v", err)
	}
	deleteNodeThroughController(t, backend, cfg.secretRoot, spec.NodeID)
	assertNodeAbsent(t, sandbox, proxy, network, spec.EnrollmentFile)
}

// deleteNodeThroughController invokes the real controller delete route, which
// removes the containers, the network, and the staged enrollment secret.
func deleteNodeThroughController(t *testing.T, backend *DockerBackend, secretRoot, nodeID string) {
	t.Helper()
	ctrl := NewController(Config{Backend: backend, SecretRoot: secretRoot})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("nodeID", nodeID)
	req := httptest.NewRequest(http.MethodDelete, "/internal/v1/workspace-nodes/"+nodeID, nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	ctrl.deleteNode(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("controller delete returned %d: %s", rec.Code, rec.Body.String())
	}
}

// probeJSON is one line of the probe binary's machine-readable output.
type probeJSON struct {
	Probe   string `json:"probe"`
	Allowed bool   `json:"allowed"`
	Detail  string `json:"detail"`
}

// probeOutcome is a probe run's output plus how the sandbox terminated it.
type probeOutcome struct {
	result  probeJSON
	hasJSON bool
	exit    int
	signal  syscall.Signal
	stderr  string
}

// runProbe executes one probe subcommand inside the sandbox with docker exec.
func runProbe(t *testing.T, backend *DockerBackend, container string, args ...string) probeOutcome {
	t.Helper()
	cmdArgs := append([]string{"exec", container, probeBinaryPath}, args...)
	cmd := exec.Command(backend.dockerPath, cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := probeOutcome{stderr: strings.TrimSpace(stderr.String())}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run probe %v: %v", args, err)
		}
		out.exit = exitErr.ProcessState.ExitCode()
		if wait, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
			out.signal = wait.Signal()
		}
	}
	if line := strings.TrimSpace(stdout.String()); line != "" {
		var parsed probeJSON
		if jsonErr := json.Unmarshal([]byte(line), &parsed); jsonErr == nil {
			out.result, out.hasJSON = parsed, true
		} else {
			t.Fatalf("probe %v printed undecodable output %q: %v", args, line, jsonErr)
		}
	}
	return out
}

// requireBlocked requires the operation to be denied: either the probe reported
// allowed=false or the sandbox killed it with SIGSYS.
func requireBlocked(t *testing.T, out probeOutcome, name string) {
	t.Helper()
	if out.hasJSON {
		if out.result.Allowed {
			t.Fatalf("%s was permitted inside the sandbox: %+v", name, out.result)
		}
		return
	}
	if out.signal != syscall.SIGSYS {
		t.Fatalf("%s neither reported a denial nor was killed by seccomp (exit %d, signal %v, stderr %q)", name, out.exit, out.signal, out.stderr)
	}
}

// requireSeccompKilled requires the syscall to be terminated by the seccomp
// KILL_PROCESS rule, which is the strongest denial the profile expresses.
func requireSeccompKilled(t *testing.T, out probeOutcome, name string) {
	t.Helper()
	if out.hasJSON {
		t.Fatalf("%s returned to userspace instead of being killed: %+v", name, out.result)
	}
	if out.signal != syscall.SIGSYS {
		t.Fatalf("%s exit = %d signal = %v, want SIGSYS from the seccomp KILL_PROCESS rule", name, out.exit, out.signal)
	}
}

// requireAllowed requires the probe to report a permitted operation.
func requireAllowed(t *testing.T, out probeOutcome, name string) {
	t.Helper()
	if !out.hasJSON || !out.result.Allowed {
		t.Fatalf("%s was unexpectedly blocked: %+v (exit %d, signal %v, stderr %q)", name, out.result, out.exit, out.signal, out.stderr)
	}
}

// waitForAllowed retries a probe until it is permitted so proxy startup does
// not race the assertion.
func waitForAllowed(t *testing.T, backend *DockerBackend, sandbox string, timeout time.Duration, args ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out := runProbe(t, backend, sandbox, args...)
		if out.hasJSON && out.result.Allowed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe %v never became allowed: %+v", args, out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// assertSecurityOptPrefix requires one SecurityOpt entry with the given prefix.
func assertSecurityOptPrefix(t *testing.T, opts []string, prefix string) {
	t.Helper()
	for _, opt := range opts {
		if strings.HasPrefix(opt, prefix) {
			return
		}
	}
	t.Errorf("SecurityOpt %v does not carry %q", opts, prefix)
}

// assertSeccompOpt accepts either the profile path or Docker's embedded copy of
// the profile JSON, which is what the daemon stores for a custom profile.
func assertSeccompOpt(t *testing.T, opts []string, path string) {
	t.Helper()
	for _, opt := range opts {
		if !strings.HasPrefix(opt, "seccomp=") {
			continue
		}
		value := strings.TrimPrefix(opt, "seccomp=")
		if value == path || strings.Contains(value, "\"defaultAction\"") {
			return
		}
	}
	t.Errorf("SecurityOpt %v does not carry a seccomp profile", opts)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func networkKeys(networks map[string]ContainerEndpoint) []string {
	keys := make([]string, 0, len(networks))
	for key := range networks {
		keys = append(keys, key)
	}
	return keys
}

// assertNodeAbsent requires no container, network, or enrollment secret with the
// node's generated names to remain.
func assertNodeAbsent(t *testing.T, sandbox, proxy, network, secretPath string) {
	t.Helper()
	containers := listNames(t, "ps", "-a", "--format", "{{.Names}}")
	for _, name := range []string{sandbox, proxy} {
		if containers[name] {
			t.Errorf("container %s survived rollback or delete", name)
		}
	}
	networks := listNames(t, "network", "ls", "--format", "{{.Name}}")
	if networks[network] {
		t.Errorf("network %s survived rollback or delete", network)
	}
	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Errorf("enrollment secret %s survived (stat err %v)", secretPath, err)
	}
}

func listNames(t *testing.T, args ...string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, line := range strings.Split(mustDocker(t, args...), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names[line] = true
		}
	}
	return names
}

// ensureUplinkNetwork creates the fleet uplink if the operator has not, and
// reports whether this test owns it.
func ensureUplinkNetwork(t *testing.T, name string) (gateway string, created bool) {
	t.Helper()
	if _, _, err := dockerAttempt(context.Background(), "network", "inspect", name); err == nil {
		return networkGateway(t, name), false
	}
	mustDocker(t, "network", "create", "--label", fleetLabel, name)
	return networkGateway(t, name), true
}

// networkGateway returns the network's gateway, deriving the first usable host
// from the subnet when Docker reports no gateway for an internal network.
func networkGateway(t *testing.T, network string) string {
	t.Helper()
	if gateway := strings.TrimSpace(mustDocker(t, "network", "inspect", "--format", "{{(index .IPAM.Config 0).Gateway}}", network)); gateway != "" {
		return gateway
	}
	subnet := strings.TrimSpace(mustDocker(t, "network", "inspect", "--format", "{{(index .IPAM.Config 0).Subnet}}", network))
	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		t.Fatalf("network %s has neither a gateway nor a parsable subnet %q", network, subnet)
	}
	v4 := ip.To4()
	if v4 == nil {
		t.Fatalf("network %s subnet %q is not IPv4", network, subnet)
	}
	v4[3]++
	return v4.String()
}

// startFakeOrigin serves the exact Multica origin the sidecar may reach over
// plain HTTP. It binds all interfaces so the sidecar reaches it through the host
// gateway.
func startFakeOrigin(t *testing.T, host string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen for the fake Multica origin: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("aurora-fake-origin"))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// startPeerContainer runs the fixture image as another workspace's listener and
// waits until it accepts connections from the host.
func startPeerContainer(t *testing.T, image, name, network, port string) string {
	t.Helper()
	mustDocker(t, "run", "-d", "--pull", "never", "--name", name, "--network", network, image, "listen", port)
	t.Cleanup(func() { _, _, _ = dockerAttempt(context.Background(), "rm", "-f", name) })
	ip := strings.TrimSpace(mustDocker(t, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name))
	if ip == "" {
		t.Fatalf("peer %s has no address on %s", name, network)
	}
	waitForTCP(t, net.JoinHostPort(ip, port))
	return ip
}

func waitForTCP(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer %s never accepted connections", address)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// writeFailOnceShim writes a docker wrapper that fails the first invocation whose
// argument string contains match. When createThenFail is set the wrapper first
// runs the real command, so the resource exists before the injected failure.
func writeFailOnceShim(t *testing.T, realDocker, match string, createThenFail bool) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "fired")
	create := ""
	if createThenFail {
		create = "      " + realDocker + ` "$@"`
	}
	script := fmt.Sprintf(shimTemplate, marker, match, marker, create, realDocker)
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write docker shim: %v", err)
	}
	return path
}

// shimTemplate is the fail-once docker wrapper. Placeholders are the marker path,
// the argument match, the marker path again, the optional pre-create command, and
// the real docker path.
const shimTemplate = `#!/bin/sh
if [ ! -f %s ]; then
  case "$*" in
    *%s*)
      : > %s
%s
      echo injected docker failure >&2
      exit 1
      ;;
  esac
fi
exec %s "$@"
`

// removeDockerResources forcibly removes the named containers, ignoring absence.
func removeDockerResources(containers ...string) {
	for _, name := range containers {
		_, _, _ = dockerAttempt(context.Background(), "rm", "-f", name)
	}
}

// dockerAttempt runs the docker CLI and returns stdout, stderr, and the error.
func dockerAttempt(ctx context.Context, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

// mustDocker runs the docker CLI and fails the test on error.
func mustDocker(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := dockerAttempt(context.Background(), args...)
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, stderr)
	}
	return stdout
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("look up %s: %v", name, err)
	}
	return path
}

// newSecuritySpec builds a unique node identity with UUIDs so the derived Docker
// names cannot collide with another run.
func newSecuritySpec(t *testing.T, secretRoot string) WorkspaceNodeSpec {
	t.Helper()
	nodeID := randomUUID(t)
	return WorkspaceNodeSpec{
		NodeID:         nodeID,
		WorkspaceID:    randomUUID(t),
		RuntimeID:      randomUUID(t),
		DaemonID:       randomUUID(t),
		EnrollmentFile: filepath.Join(secretRoot, nodeID, "enrollment"),
	}
}

// writeEnrollmentFile stages one uniquely valued enrollment secret and returns
// its raw value so the caller can scan for leaks.
func writeEnrollmentFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create secret dir: %v", err)
	}
	secret := "mse_" + randomHex(t, 20)
	if err := os.WriteFile(path, []byte(secret), 0o400); err != nil {
		t.Fatalf("write enrollment secret: %v", err)
	}
	return secret
}

func randomUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		t.Fatalf("random UUID: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("random hex: %v", err)
	}
	return hex.EncodeToString(b)
}

// pinnedDigest extracts the immutable digest from a digest-pinned reference.
func pinnedDigest(t *testing.T, image string) string {
	t.Helper()
	at := strings.LastIndex(image, "@")
	if at < 0 {
		t.Fatalf("image %q is not digest-pinned", image)
	}
	return image[at+1:]
}
