package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// auroraEnrollmentToken is a well-formed server-issued enrollment secret.
const auroraEnrollmentToken = "mse_0123456789abcdef0123456789abcdef01234567"

// testSeccompProfileJSON is a minimal well-formed operator seccomp profile. The
// unit tests inline its exact bytes into HostConfig.SecurityOpt and write it to
// a temp file when a test drives Provider.Ensure, which reads the profile.
const testSeccompProfileJSON = `{"defaultAction":"SCMP_ACT_ALLOW","architectures":["SCMP_ARCH_AARCH64","SCMP_ARCH_X86_64"],"syscalls":[]}`

// auroraConfigWithSeccomp returns the Aurora unit config with a real, valid
// seccomp profile. Tests that call Provider.Ensure need the profile to exist
// because Ensure now reads and inlines it.
func auroraConfigWithSeccomp(t *testing.T) model.Config {
	t.Helper()
	cfg := auroraConfig()
	path := filepath.Join(t.TempDir(), "seccomp.json")
	if err := os.WriteFile(path, []byte(testSeccompProfileJSON), 0o600); err != nil {
		t.Fatalf("write seccomp profile: %v", err)
	}
	cfg.Aurora.SeccompProfile = path
	return cfg
}

func auroraConfig() model.Config {
	cfg := fixtureConfig()
	cfg.Aurora = &model.AuroraConfig{
		ServerURL:        "http://api.internal:8080",
		ProxyImage:       "ghcr.io/eanfs/multica-aurora-egress@sha256:b123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SeccompProfile:   "/etc/multica/aurora/seccomp.json",
		AppArmorProfile:  "multica-aurora-sandbox",
		EgressHosts:      []string{"api.anthropic.com:443"},
		AnthropicBaseURL: "https://ark.example.com",
		AnthropicModel:   "ark-model",
		ProviderSecretFiles: map[string]string{
			"anthropic-api-key": "/etc/multica/aurora/anthropic-api-key",
		},
		ReadonlyRootfs: true,
		UplinkNetwork:  "aurora-egress-uplink",
	}
	return cfg
}

func auroraNode() model.Node {
	n := fixtureNode()
	n.Maintenance = false
	n.ContainerID = ""
	return n
}

func auroraBootstrap(n model.Node) model.Bootstrap {
	return model.Bootstrap{EnrollmentToken: auroraEnrollmentToken, ServerURL: "http://api.internal:8080", DaemonID: n.DaemonID}
}

// TestProviderEnsureAuroraProfile drives the full Aurora admission: an owned
// internal workspace network, one credential-free egress sidecar on the uplink
// network, and a sandbox node that only joins the workspace network.
func TestProviderEnsureAuroraProfile(t *testing.T) {
	cfg := auroraConfigWithSeccomp(t)
	n := auroraNode()
	base := New(fakeCalls{}, cfg)
	nodeName := base.containerName(n)
	egressName := base.egressName(n)
	workspace := base.workspaceNetwork(n).Name

	type createCall struct {
		cfg  *container.Config
		host container.HostConfig
		net  string
		name string
	}
	var creates []createCall
	var connected []string
	var networks []Resource
	inspect := func(_ context.Context, id string) (Inspection, error) {
		switch id {
		case "egress-id":
			return Inspection{ID: "egress-id", State: "created", Labels: labels(n.Namespace, cfg.FleetID, nodeID(n), egressProxyRole)}, nil
		case "node-id":
			return Inspection{ID: "node-id", State: "created", Labels: labels(n.Namespace, cfg.FleetID, nodeID(n), "node")}, nil
		default:
			return Inspection{}, errdefs.ErrNotFound
		}
	}
	e := fakeCalls{
		inspect:       inspect,
		ensureNetwork: func(_ context.Context, r Resource) error { networks = append(networks, r); return nil },
		ensureVolume:  func(context.Context, Resource) error { return nil },
		bootstrap:     func(context.Context, []Resource, []byte) error { return nil },
		connectNetwork: func(_ context.Context, net, id string, aliases []string) error {
			connected = append(connected, net+"|"+id+"|"+strings.Join(aliases, ","))
			return nil
		},
		removeNetwork: func(context.Context, Resource) error { return nil },
		create: func(_ context.Context, c *container.Config, h *container.HostConfig, net, name string) (string, error) {
			creates = append(creates, createCall{cfg: c, host: *h, net: net, name: name})
			if name == egressName {
				return "egress-id", nil
			}
			return "node-id", nil
		},
		start:  func(context.Context, string) error { return nil },
		health: func(context.Context, string) ([]byte, error) { return []byte(goodHealth), nil },
	}
	p := New(e, cfg)
	if _, err := p.Ensure(context.Background(), n, auroraBootstrap(n)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(networks) != 2 || networks[0].Internal || !networks[1].Internal || networks[1].Role != "workspace-network" || networks[1].Name != workspace {
		t.Fatalf("networks = %+v", networks)
	}
	if len(creates) != 2 {
		t.Fatalf("creates = %d", len(creates))
	}
	proxy, node := creates[0], creates[1]
	if proxy.name != egressName || proxy.net != cfg.Aurora.UplinkNetwork || proxy.cfg.Image != cfg.Aurora.ProxyImage {
		t.Fatalf("egress create = %+v", proxy)
	}
	if len(proxy.host.Mounts) != 0 || len(proxy.cfg.Env) != 2 || proxy.host.ReadonlyRootfs == false || proxy.host.NetworkMode != container.NetworkMode(cfg.Aurora.UplinkNetwork) {
		t.Fatalf("egress host = %+v env=%v", proxy.host, proxy.cfg.Env)
	}
	if strings.Contains(strings.Join(proxy.cfg.Env, " "), "API_KEY=") {
		t.Fatalf("sidecar carries a credential: %v", proxy.cfg.Env)
	}
	if node.name != nodeName || node.net != workspace {
		t.Fatalf("node create = %+v", node)
	}
	wantEnv := []string{
		"HOME=" + model.NodeHome,
		"FLEET_NODE_MAX_RUNS=1",
		"MULTICA_MANAGED=1",
		"MULTICA_SERVER_URL=http://api.internal:8080",
		"MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE=" + model.AuroraEnrollmentFile,
		"HTTP_PROXY=" + model.AuroraEgressProxyEndpoint,
		"HTTPS_PROXY=" + model.AuroraEgressProxyEndpoint,
		"NO_PROXY=" + model.AuroraNoProxyValue,
		"MULTICA_CLAUDE_PATH=" + model.AuroraClaudePath,
		"ANTHROPIC_BASE_URL=https://ark.example.com",
		"ANTHROPIC_MODEL=ark-model",
	}
	if !reflect.DeepEqual(node.cfg.Env, wantEnv) {
		t.Fatalf("node env = %v", node.cfg.Env)
	}
	if node.host.NetworkMode != container.NetworkMode(workspace) || !node.host.ReadonlyRootfs || !reflect.DeepEqual(node.host.SecurityOpt, []string{"no-new-privileges:true", "seccomp=" + testSeccompProfileJSON, "apparmor=" + cfg.Aurora.AppArmorProfile}) {
		t.Fatalf("node host = %+v", node.host)
	}
	if len(node.host.Mounts) != 3 || node.host.Mounts[2].Type != "bind" || !node.host.Mounts[2].ReadOnly || node.host.Mounts[2].Target != model.AuroraAnthropicAPIKeyTarget {
		t.Fatalf("node mounts = %+v", node.host.Mounts)
	}
	if len(connected) != 1 || connected[0] != workspace+"|egress-id|"+model.AuroraEgressAlias {
		t.Fatalf("connect = %v", connected)
	}
	if !reflect.DeepEqual([]string(node.cfg.Entrypoint), []string{"/usr/local/bin/fleet-node"}) || !reflect.DeepEqual([]string(node.cfg.Cmd), []string{"run"}) {
		t.Fatalf("entrypoint changed: %v %v", node.cfg.Entrypoint, node.cfg.Cmd)
	}
}

func TestProviderRejectsCrossProfileBootstrap(t *testing.T) {
	n := auroraNode()
	notFound := fakeCalls{inspect: func(context.Context, string) (Inspection, error) { return Inspection{}, errdefs.ErrNotFound }}
	aurora := New(notFound, auroraConfig())
	if _, err := aurora.Ensure(context.Background(), n, model.Bootstrap{NodeToken: "mcn_fake", APIKey: "fake", ServerURL: "http://api.internal:8080", DaemonID: n.DaemonID}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("claude bootstrap accepted under the aurora profile: %v", err)
	}
	claude := New(notFound, fixtureConfig())
	if _, err := claude.Ensure(context.Background(), n, model.Bootstrap{EnrollmentToken: auroraEnrollmentToken, ServerURL: fixtureConfig().APIURL, DaemonID: n.DaemonID}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("enrollment secret accepted under the claude profile: %v", err)
	}
	if _, err := claude.Ensure(context.Background(), n, auroraBootstrap(n)); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("aurora payload accepted under the claude profile: %v", err)
	}
}

func TestInstallerTarSelectsProfilePayload(t *testing.T) {
	n := auroraNode()
	aurora, err := installerTar(n, auroraConfig(), auroraBootstrap(n))
	if err != nil || !strings.Contains(string(aurora), "secrets/aurora-enrollment") || strings.Contains(string(aurora), "bootstrap.json") {
		t.Fatalf("aurora installer err=%v", err)
	}
	claude, err := installerTar(n, fixtureConfig(), model.Bootstrap{NodeToken: "mcn_fake", APIKey: "fake", ServerURL: fixtureConfig().APIURL, DaemonID: n.DaemonID})
	if err != nil || !strings.Contains(string(claude), "secrets/bootstrap.json") || strings.Contains(string(claude), "aurora-enrollment") {
		t.Fatalf("claude installer err=%v", err)
	}
	if _, err := installerTar(n, auroraConfig(), model.Bootstrap{EnrollmentToken: "nope", DaemonID: n.DaemonID}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("invalid enrollment token accepted: %v", err)
	}
}

func TestAuroraBootstrapValidationRejectsForeignPayload(t *testing.T) {
	n := auroraNode()
	raw, err := installerTar(n, auroraConfig(), auroraBootstrap(n))
	if err != nil {
		t.Fatal(err)
	}
	data := Resource{Role: "secrets", Labels: map[string]string{"multica.fleet.node": fixtureLabels("node")["multica.fleet.node"]}}
	engine := &sdkEngine{cfg: auroraConfig()}
	if err := engine.validateBootstrap(raw, data); err != nil {
		t.Fatalf("canonical aurora installer rejected: %v", err)
	}
	claudeRaw, err := installerTar(n, fixtureConfig(), model.Bootstrap{NodeToken: "mcn_fake", APIKey: "fake", ServerURL: fixtureConfig().APIURL, DaemonID: n.DaemonID})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateBootstrap(claudeRaw, data); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("claude installer accepted by the aurora profile: %v", err)
	}
}

func TestInspectAuroraEnvironment(t *testing.T) {
	base := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + model.NodeHome, "FLEET_NODE_MAX_RUNS=1"}
	managed := []string{"MULTICA_MANAGED=1", "MULTICA_SERVER_URL=http://api.internal:8080", "MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE=" + model.AuroraEnrollmentFile}
	proxy := []string{"HTTP_PROXY=" + model.AuroraEgressProxyEndpoint, "HTTPS_PROXY=" + model.AuroraEgressProxyEndpoint, "NO_PROXY=" + model.AuroraNoProxyValue}
	agent := []string{"MULTICA_CLAUDE_PATH=" + model.AuroraClaudePath}
	full := append(append(append(append(append([]string{}, base...), managed...), proxy...), agent...), "ANTHROPIC_BASE_URL=https://ark.example.com", "ANTHROPIC_MODEL=ark-model")
	if !inspectEnvironment(full, 1, auroraConfig().Aurora) {
		t.Fatal("valid aurora environment rejected")
	}
	if inspectEnvironment(full, 1, nil) {
		t.Fatal("aurora environment accepted without the profile")
	}
	if inspectEnvironment(base, 1, auroraConfig().Aurora) {
		t.Fatal("missing managed enrollment environment accepted")
	}
	noProxy := append(append([]string{}, base...), managed...)
	if inspectEnvironment(noProxy, 1, auroraConfig().Aurora) {
		t.Fatal("missing egress proxy environment accepted")
	}
	// The provider supplies the agent path, so an adopted container that omits
	// it would let the daemon fall back to a PATH lookup the image no longer
	// satisfies.
	noAgent := append(append(append([]string{}, base...), managed...), proxy...)
	noAgent = append(noAgent, "ANTHROPIC_BASE_URL=https://ark.example.com", "ANTHROPIC_MODEL=ark-model")
	if inspectEnvironment(noAgent, 1, auroraConfig().Aurora) {
		t.Fatal("aurora environment without the provider agent path accepted")
	}
	// The neutral image no longer carries the Node base banners; a container
	// that presents them is drift and must fail closed.
	for _, banner := range []string{"NODE_VERSION=22.23.3", "YARN_VERSION=1.22.22"} {
		if inspectEnvironment(append(append([]string{}, full...), banner), 1, auroraConfig().Aurora) {
			t.Fatalf("aurora environment with baked %s accepted", banner)
		}
	}
	credentialed := append(append([]string{}, base...), "MULTICA_MANAGED=1", "MULTICA_SERVER_URL=http://user:pass@api.internal:8080", "MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE="+model.AuroraEnrollmentFile)
	credentialed = append(credentialed, proxy...)
	credentialed = append(credentialed, agent...)
	if inspectEnvironment(credentialed, 1, auroraConfig().Aurora) {
		t.Fatal("credentialed server url accepted")
	}
}

// TestInspectAuroraEnvironmentRejectsConfigDrift pins the adoption authority's
// config-derived environment comparison: the values nodeEnv builds from
// cfg.Aurora must match the live container exactly, and an ANTHROPIC_* variable
// the config does not carry is rejected rather than structurally accepted.
func TestInspectAuroraEnvironmentRejectsConfigDrift(t *testing.T) {
	cfg := auroraConfig()
	canonical := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + model.NodeHome,
		"FLEET_NODE_MAX_RUNS=1",
		model.AuroraManagedEnv + "=1",
		model.AuroraServerURLEnv + "=" + cfg.Aurora.ServerURL,
		model.AuroraEnrollmentFileEnv + "=" + model.AuroraEnrollmentFile,
		model.AuroraHTTPProxyEnv + "=" + model.AuroraEgressProxyEndpoint,
		model.AuroraHTTPSProxyEnv + "=" + model.AuroraEgressProxyEndpoint,
		model.AuroraNoProxyEnv + "=" + model.AuroraNoProxyValue,
		model.AuroraClaudePathEnv + "=" + model.AuroraClaudePath,
		model.AuroraAnthropicBaseURLEnv + "=" + cfg.Aurora.AnthropicBaseURL,
		model.AuroraAnthropicModelEnv + "=" + cfg.Aurora.AnthropicModel,
	}
	withValue := func(key, value string) []string {
		out := append([]string(nil), canonical...)
		for i, entry := range out {
			if strings.HasPrefix(entry, key+"=") {
				out[i] = key + "=" + value
				return out
			}
		}
		return append(out, key+"="+value)
	}
	without := func(keys ...string) []string {
		drop := map[string]bool{}
		for _, k := range keys {
			drop[k] = true
		}
		out := make([]string, 0, len(canonical))
		for _, entry := range canonical {
			k, _, _ := strings.Cut(entry, "=")
			if !drop[k] {
				out = append(out, entry)
			}
		}
		return out
	}

	if !inspectEnvironment(canonical, 1, cfg.Aurora) {
		t.Fatal("canonical aurora environment rejected")
	}
	if inspectEnvironment(withValue(model.AuroraServerURLEnv, "http://other.internal:9090"), 1, cfg.Aurora) {
		t.Fatal("adopted a node built for a different server url")
	}
	if inspectEnvironment(withValue(model.AuroraAnthropicBaseURLEnv, "https://other.example.com"), 1, cfg.Aurora) {
		t.Fatal("adopted a node built for a different anthropic base url")
	}
	if inspectEnvironment(withValue(model.AuroraAnthropicModelEnv, "other-model"), 1, cfg.Aurora) {
		t.Fatal("adopted a node built for a different anthropic model")
	}
	if inspectEnvironment(without(model.AuroraAnthropicBaseURLEnv), 1, cfg.Aurora) {
		t.Fatal("adopted a node missing its configured anthropic base url")
	}
	if inspectEnvironment(without(model.AuroraAnthropicModelEnv), 1, cfg.Aurora) {
		t.Fatal("adopted a node missing its configured anthropic model")
	}
	if inspectEnvironment(withValue(model.AuroraClaudePathEnv, "/opt/other/claude"), 1, cfg.Aurora) {
		t.Fatal("adopted a node with a non-provider agent path")
	}
	if inspectEnvironment(without(model.AuroraClaudePathEnv), 1, cfg.Aurora) {
		t.Fatal("adopted a node missing the provider agent path")
	}
	// NODE_VERSION/YARN_VERSION come from a Node base image, which the neutral
	// final stage no longer uses; either one is forbidden drift.
	if inspectEnvironment(append(append([]string{}, canonical...), "NODE_VERSION=22.23.3"), 1, cfg.Aurora) {
		t.Fatal("adopted a node carrying NODE_VERSION")
	}
	if inspectEnvironment(append(append([]string{}, canonical...), "YARN_VERSION=1.22.22"), 1, cfg.Aurora) {
		t.Fatal("adopted a node carrying YARN_VERSION")
	}

	// A deployment that carries no override must reject ANTHROPIC_* entirely,
	// and must likewise accept a node that has neither variable.
	noOverride := *cfg.Aurora
	noOverride.AnthropicBaseURL = ""
	noOverride.AnthropicModel = ""
	if inspectEnvironment(canonical, 1, &noOverride) {
		t.Fatal("ANTHROPIC_* accepted when the config carries none")
	}
	if !inspectEnvironment(without(model.AuroraAnthropicBaseURLEnv, model.AuroraAnthropicModelEnv), 1, &noOverride) {
		t.Fatal("override-free config rejected a node without anthropic variables")
	}
}
