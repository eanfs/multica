package aurorafleet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The sandbox container runs as this non-root user. It matches the user the
// sandbox image is built for; there is no configuration surface to change it.
const sandboxUser = "10001:10001"

// Node name prefixes are the single source of truth for every policy-generated
// Docker resource name. NodeNames builds names from them and the backend derives
// siblings from a sandbox name by swapping the prefix, so the two can never drift.
const (
	sandboxNamePrefix = "aurora-sbx-"
	proxyNamePrefix   = "aurora-egr-"
	networkNamePrefix = "aurora-ws-"
)

// egressProxyEndpoint is the address the sandbox uses to reach its egress
// sidecar across the per-workspace internal network. "egress" is the alias
// Docker assigns when the sidecar is connected to the workspace network.
const egressProxyEndpoint = "http://egress:3128"

// appArmorProfile is the AppArmor profile name deployed as
// deploy/aurora-sandbox/multica-aurora-sandbox.apparmor. Loading it is an
// operator step; enforcement here is the --security-opt reference.
const appArmorProfile = "multica-aurora-sandbox"

// uplinkNetworkName is the fleet-wide Docker network that carries outbound
// traffic. Only the egress sidecar joins it; the sandbox never does.
const uplinkNetworkName = "aurora-egress-uplink"

// enrollmentSecretMountPath is where the controller-staged enrollment secret
// appears inside the sandbox. It is always mounted read-only.
const enrollmentSecretMountPath = "/run/secrets/aurora-enrollment"

// Fixed provider credential destinations inside the sandbox. The daemon reads
// the Anthropic value from its child environment; the MCP broker reads the
// other three files directly. The fleet mounts every one read-only at exactly
// these paths and never passes a credential value.
const (
	anthropicAPIKeyMountPath = "/run/secrets/anthropic-api-key"
	arkAPIKeyMountPath       = "/run/secrets/ark-api-key"
	openaiAPIKeyMountPath    = "/run/secrets/openai-api-key"
	volcASRAPIKeyMountPath   = "/run/secrets/volc-asr-api-key"
)

// ProviderSecretFiles are the operator-staged host files mounted at the four
// fixed provider credential destinations. They are policy configuration, not
// request data: the control API cannot choose either a source or a
// destination. Each non-empty path must live under Policy.SecretRoot.
type ProviderSecretFiles struct {
	AnthropicAPIKey string
	ArkAPIKey       string
	OpenAIAPIKey    string
	VolcASRAPIKey   string
}

// compiledEgressHosts are the provider endpoints every workspace may reach
// through the egress sidecar. The list is compiled into the policy, not
// configurable per workspace.
var compiledEgressHosts = []string{
	"api.anthropic.com:443",
	"ark.cn-beijing.volces.com:443",
	"api.openai.com:443",
	"openspeech.bytedance.com:443",
}

// digestPattern matches an immutable OCI digest: sha256 plus exactly 64
// lowercase hex characters. Tag-only or uppercase references are rejected.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// safeNamePattern constrains generated Docker network/container names to a
// conservative charset and length before any docker invocation.
var safeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// labelNamespace is the only label key namespace this fleet writes.
const labelNamespace = "com.multica.aurora."

// nodeIdentityLabel carries the node UUID on every fleet-owned sandbox. It is
// the controlled identity a delete can use to resolve a node to its container.
const nodeIdentityLabel = labelNamespace + "node"

// Policy is the immutable hardened Docker policy for workspace sandbox nodes.
// Every field is process configuration owned by the operator; API callers can
// never influence any of it, and the argv builders below turn it into a
// deterministic docker invocation. The zero value is not usable: Validate
// fails closed so an unconfigured backend never provisions anything.
type Policy struct {
	// SandboxImage must be digest-pinned (@sha256:<64 lowercase hex>).
	SandboxImage string
	// ProxyImage must be digest-pinned (@sha256:<64 lowercase hex>).
	ProxyImage string
	// SeccompPath is the absolute host path of the deployed seccomp profile
	// (deploy/aurora-sandbox/seccomp.json).
	SeccompPath string
	// ServerOrigin is the exact Multica server origin sandboxes call back to.
	ServerOrigin string
	// SecretRoot is the host directory under which enrollment secrets are
	// staged. Only files inside this directory are mountable.
	SecretRoot string
	// ExtraEgressHosts are additional exact host:port entries the operator
	// allows through the egress sidecar. Wildcards are rejected.
	ExtraEgressHosts []string
	// ProviderSecretFiles are the operator-staged provider credential files
	// mounted read-only at their fixed destinations. Empty entries mount
	// nothing; API callers cannot influence this value.
	ProviderSecretFiles ProviderSecretFiles
}

// Validate reports whether the policy is completely and safely configured.
func (p Policy) Validate() error {
	for name, image := range map[string]string{"sandbox": p.SandboxImage, "proxy": p.ProxyImage} {
		if err := validateDigestPinnedImage(image); err != nil {
			return fmt.Errorf("%s image: %w", name, err)
		}
	}
	if !filepath.IsAbs(p.SeccompPath) {
		return fmt.Errorf("seccomp path %q must be absolute", p.SeccompPath)
	}
	if !filepath.IsAbs(p.SecretRoot) {
		return fmt.Errorf("secret root %q must be absolute", p.SecretRoot)
	}
	if err := validateOrigin(p.ServerOrigin); err != nil {
		return fmt.Errorf("server origin: %w", err)
	}
	for _, h := range p.ExtraEgressHosts {
		if h == "" || strings.ContainsAny(h, "*? 	") || !strings.Contains(h, ":") {
			return fmt.Errorf("extra egress host %q must be an exact host:port", h)
		}
	}
	return nil
}

// UplinkNetwork is the fleet-wide outbound network only the egress sidecar
// joins.
func (p Policy) UplinkNetwork() string { return uplinkNetworkName }

// NodeNames derives the deterministic network, egress sidecar, and sandbox
// container names for one workspace node. All names are built from a SHA-256
// prefix of the workspace and node IDs, so workspace-controlled strings can
// never appear in a docker invocation; any identity that is not a canonical
// UUID is rejected before hashing.
func (p Policy) NodeNames(spec WorkspaceNodeSpec) (network, proxy, sandbox string, err error) {
	for name, id := range map[string]string{
		"node_id": spec.NodeID, "workspace_id": spec.WorkspaceID,
		"runtime_id": spec.RuntimeID, "daemon_id": spec.DaemonID,
	} {
		if !uuidPattern.MatchString(id) {
			return "", "", "", fmt.Errorf("%s must be a UUID, got %q", name, id)
		}
	}
	sum := sha256.Sum256([]byte("workspace=" + spec.WorkspaceID + ";node=" + spec.NodeID))
	prefix := hex.EncodeToString(sum[:])[:16]
	network, proxy, sandbox = networkNamePrefix+prefix, proxyNamePrefix+prefix, sandboxNamePrefix+prefix
	for _, name := range []string{network, proxy, sandbox} {
		if !safeNamePattern.MatchString(name) {
			return "", "", "", fmt.Errorf("generated name %q is not a safe Docker name", name)
		}
	}
	return network, proxy, sandbox, nil
}

// WorkspaceNetworkCreateArgs returns the docker argv that creates the
// workspace-private network. It is created --internal: no route to the host,
// the outside world, or other workspaces exists except through the egress
// sidecar.
func (p Policy) WorkspaceNetworkCreateArgs(network string) []string {
	return []string{"network", "create", "--internal", "--label", fleetLabel, network}
}

// SandboxArgs returns the full hardened docker run argv for the sandbox
// container. The security flags are constants in this file, not request
// data; the spec contributes identity only.
func (p Policy) SandboxArgs(spec WorkspaceNodeSpec) ([]string, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("sandbox policy: %w", err)
	}
	mount, err := p.secretMountArgs(spec.EnrollmentFile)
	if err != nil {
		return nil, err
	}
	providerMounts, err := p.providerSecretMountArgs()
	if err != nil {
		return nil, err
	}
	network, _, sandbox, err := p.NodeNames(spec)
	if err != nil {
		return nil, err
	}
	labels, err := identityLabels(spec)
	if err != nil {
		return nil, err
	}

	args := []string{
		"run", "--detach",
		"--pull", "never",
		"--init",
		"--restart", "no",
		"--stop-timeout", "30",
		"--name", sandbox,
		"--user", sandboxUser,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--security-opt", "seccomp=" + p.SeccompPath,
		"--security-opt", "apparmor=" + appArmorProfile,
		"--pids-limit", "256",
		"--memory", "4g",
		"--memory-swap", "4g",
		"--cpus", "2",
		"--ulimit", "nofile=1024:1024",
		// Every writable tmpfs is nosuid,nodev,noexec: files that must
		// execute are installed in the read-only image.
		"--tmpfs", "/workspace:rw,nosuid,nodev,noexec,size=2147483648,uid=10001,gid=10001,mode=0700",
		"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=268435456,uid=10001,gid=10001,mode=0700",
		"--tmpfs", "/run:rw,nosuid,nodev,noexec,size=16777216,uid=10001,gid=10001,mode=0755",
		"--network", network,
	}
	args = append(args, mount...)
	args = append(args, providerMounts...)
	args = append(args, envArgs(map[string]string{
		"MULTICA_SERVER_URL":                    p.ServerOrigin,
		"MULTICA_MANAGED":                       "1",
		"MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE": enrollmentSecretMountPath,
		"MULTICA_AGENT_TIMEOUT":                 "30m",
		"HTTP_PROXY":                            egressProxyEndpoint,
		"HTTPS_PROXY":                           egressProxyEndpoint,
		"NO_PROXY":                              "egress,127.0.0.1,localhost",
	})...)
	args = append(args, labelArgs(labels)...)
	// The image is the final argument so nothing can follow it as a command.
	return append(args, p.SandboxImage), nil
}

// ProxyArgs returns the hardened docker run argv for the egress sidecar. The
// sidecar receives only non-secret configuration (the exact server origin and
// the allowed host list) and mounts no enrollment or provider credential.
func (p Policy) ProxyArgs(proxyName string) ([]string, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("proxy policy: %w", err)
	}
	hosts := append([]string{}, compiledEgressHosts...)
	hosts = append(hosts, p.ExtraEgressHosts...)

	args := []string{
		"run", "--detach",
		"--pull", "never",
		"--init",
		"--restart", "no",
		"--name", proxyName,
		"--user", sandboxUser,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--pids-limit", "64",
		"--memory", "256m",
		"--memory-swap", "256m",
		"--cpus", "0.25",
		"--ulimit", "nofile=512:512",
		"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=33554432,uid=10001,gid=10001,mode=0700",
		// Created on the uplink network; the workspace-internal network is
		// attached afterwards (EgressNetworkConnect) so "egress" resolves
		// from the sandbox.
		"--network", p.UplinkNetwork(),
	}
	args = append(args, envArgs(map[string]string{
		"MULTICA_EGRESS_SERVER_ORIGIN": p.ServerOrigin,
		"MULTICA_EGRESS_ALLOWED_HOSTS": strings.Join(hosts, ","),
	})...)
	args = append(args, labelArgs(map[string]string{
		labelNamespace + "role": "egress-proxy",
	})...)
	// No --mount of any kind: the sidecar holds no credentials.
	return append(args, p.ProxyImage), nil
}

// EgressNetworkConnect returns the docker argv that attaches the egress
// sidecar to the workspace-internal network under the "egress" alias.
func (p Policy) EgressNetworkConnect(proxyName, network string) []string {
	return []string{"network", "connect", "--alias", "egress", network, proxyName}
}

// secretMountArgs returns the read-only bind mount for the controller-staged
// enrollment secret file.
func (p Policy) secretMountArgs(path string) ([]string, error) {
	return p.secretMountArgsAt(path, enrollmentSecretMountPath)
}

// secretMountArgsAt returns the read-only bind mount for one controller-staged
// secret file. The path must be absolute, a regular file (never a symlink), and
// inside the configured secret root.
func (p Policy) secretMountArgsAt(path, destination string) ([]string, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("secret path %q must be absolute", path)
	}
	clean := filepath.Clean(path)
	root := filepath.Clean(p.SecretRoot)
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("secret path %q is outside the secret root %q", path, p.SecretRoot)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, fmt.Errorf("stat secret file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("secret file %q is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("secret path %q is not a regular file", path)
	}
	return []string{
		"--mount", "type=bind,src=" + clean + ",dst=" + destination + ",readonly",
	}, nil
}

// providerSecretMountArgs mounts the operator-staged provider credential files
// read-only at their four fixed destinations. Each supplied path is validated
// against SecretRoot, so the control API can never point the sandbox at a
// credential outside the operator's secret directory; an empty entry mounts
// nothing and the daemon's own startup validation rejects the run.
func (p Policy) providerSecretMountArgs() ([]string, error) {
	ordered := []struct {
		path        string
		destination string
	}{
		{p.ProviderSecretFiles.AnthropicAPIKey, anthropicAPIKeyMountPath},
		{p.ProviderSecretFiles.ArkAPIKey, arkAPIKeyMountPath},
		{p.ProviderSecretFiles.OpenAIAPIKey, openaiAPIKeyMountPath},
		{p.ProviderSecretFiles.VolcASRAPIKey, volcASRAPIKeyMountPath},
	}
	var mounts []string
	for _, entry := range ordered {
		if strings.TrimSpace(entry.path) == "" {
			continue
		}
		mount, err := p.secretMountArgsAt(entry.path, entry.destination)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount...)
	}
	return mounts, nil
}

// identityLabels returns the controlled identity labels for a sandbox node.
// Labels carry identity only — never tokens, prompts, user names, provider
// URLs, or provider credentials.
func identityLabels(spec WorkspaceNodeSpec) (map[string]string, error) {
	labels := map[string]string{
		labelNamespace + "managed":   "1",
		labelNamespace + "role":      "sandbox",
		nodeIdentityLabel:            spec.NodeID,
		labelNamespace + "workspace": spec.WorkspaceID,
		labelNamespace + "runtime":   spec.RuntimeID,
		labelNamespace + "daemon":    spec.DaemonID,
	}
	for k, v := range labels {
		if !safeLabelValue(v) {
			return nil, fmt.Errorf("label %s value is not a safe identity string", k)
		}
	}
	return labels, nil
}

// safeLabelValue restricts label values to UUIDs and short fixed tokens
// ("sandbox", "egress-proxy", "1"); it excludes spaces, shell metacharacters,
// and docker-flag lookalikes.
var safeLabelValuePattern = regexp.MustCompile(`^[0-9a-zA-Z-]{1,64}$`)

func safeLabelValue(v string) bool {
	return safeLabelValuePattern.MatchString(v)
}

// mergeLabels adds key/value pairs to labels, rejecting duplicate keys and
// any key outside the controlled label namespace.
func mergeLabels(labels map[string]string, pairs ...string) error {
	if len(pairs)%2 != 0 {
		return errors.New("odd number of label key/value pairs")
	}
	for i := 0; i < len(pairs); i += 2 {
		k, v := pairs[i], pairs[i+1]
		if _, dup := labels[k]; dup {
			return fmt.Errorf("duplicate label key %q", k)
		}
		if !strings.HasPrefix(k, labelNamespace) {
			return fmt.Errorf("label key %q is outside the %s namespace", k, labelNamespace)
		}
		labels[k] = v
	}
	return nil
}

// envArgs flattens an environment map into deterministic "-e K=V" arguments.
func envArgs(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(env))
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}
	return args
}

// labelArgs flattens a label map into deterministic "--label K=V" arguments.
func labelArgs(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(labels))
	for _, k := range keys {
		args = append(args, "--label", k+"="+labels[k])
	}
	return args
}

// validateDigestPinnedImage requires an OCI image reference whose digest is
// an immutable sha256 of exactly 64 lowercase hex characters.
func validateDigestPinnedImage(image string) error {
	at := strings.LastIndex(image, "@")
	if at <= 0 || at == len(image)-1 {
		return fmt.Errorf("image %q must be digest-pinned as <name>@sha256:<64 hex>", image)
	}
	name, digest := image[:at], image[at+1:]
	if strings.ContainsAny(name, " \t\r\n") || strings.Contains(name, "@") {
		return fmt.Errorf("image name %q is not a valid OCI reference", name)
	}
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("image %q digest must be sha256:<64 lowercase hex>", image)
	}
	return nil
}

// validateOrigin requires an http(s) origin with a host and no credentials,
// path, query, or fragment.
func validateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must be a bare http(s) origin", origin)
	}
	return nil
}
