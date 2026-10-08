package docker

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

func TestAuroraNodeHostConfigRestricted(t *testing.T) {
	spec := model.Spec{CPUs: 2, MemoryBytes: 4 << 30, Pids: 256, MaxRuns: 1}
	aurora := auroraConfig().Aurora
	h := NodeHostConfig(spec, true, aurora)
	if !h.ReadonlyRootfs || h.Privileged || h.PidMode != "" || len(h.PortBindings) != 0 || h.PublishAllPorts || h.NetworkMode == "host" {
		t.Fatalf("unsafe aurora isolation: %+v", h)
	}
	if !reflect.DeepEqual(h.SecurityOpt, []string{"no-new-privileges:true"}) {
		t.Fatalf("security opt = %v", h.SecurityOpt)
	}
	wantTmpfs := map[string]string{
		model.AuroraWorkspaceMount: auroraWorkspaceTmpfs,
		model.AuroraTmpMount:       auroraTmpTmpfs,
		model.AuroraRunMount:       auroraRunTmpfs,
	}
	if !reflect.DeepEqual(h.Tmpfs, wantTmpfs) {
		t.Fatalf("tmpfs = %v", h.Tmpfs)
	}
	for _, f := range []string{auroraWorkspaceTmpfs, auroraTmpTmpfs, auroraRunTmpfs} {
		for _, want := range []string{"nosuid", "nodev", "noexec", "uid=10001", "gid=10001"} {
			if !strings.Contains(f, want) {
				t.Fatalf("tmpfs missing %s: %s", want, f)
			}
		}
	}
	// The Claude profile must be byte-for-byte identical to the pre-Aurora builder.
	claude := NodeHostConfig(spec, true, nil)
	if claude.ReadonlyRootfs || len(claude.Tmpfs) != 0 || !reflect.DeepEqual(claude.SecurityOpt, []string{"no-new-privileges:true"}) {
		t.Fatalf("claude hostconfig changed: %+v", claude)
	}
}

func TestEgressSidecarPolicy(t *testing.T) {
	cfg := auroraConfig()
	n := auroraNode()
	proxyName := New(fakeCalls{}, cfg).egressName(n)
	workspace := New(fakeCalls{}, cfg).workspaceNetwork(n).Name
	args, err := EgressProxyArgs(cfg, proxyName, workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--network "+cfg.Aurora.UplinkNetwork) {
		t.Fatalf("sidecar not on uplink: %v", args)
	}
	if strings.Contains(joined, "mount") || strings.Contains(joined, "API_KEY") || strings.Contains(joined, "enrollment") {
		t.Fatalf("sidecar mounts a credential: %v", args)
	}
	if args[len(args)-1] != cfg.Image {
		t.Fatalf("sidecar image not final: %v", args)
	}
	if got := EgressNetworkConnectArgs(proxyName, workspace); !reflect.DeepEqual(got, []string{"network", "connect", "--alias", model.AuroraEgressAlias, workspace, proxyName}) {
		t.Fatalf("connect args = %v", got)
	}
	// The Engine-level spec encodes the same policy: uplink only, no mounts and
	// no provider credential.
	spec, host, err := egressProxySpec(cfg, n, proxyName, true)
	if err != nil {
		t.Fatal(err)
	}
	if host.NetworkMode != container.NetworkMode(cfg.Aurora.UplinkNetwork) || !host.ReadonlyRootfs || len(host.Mounts) != 0 {
		t.Fatalf("sidecar host = %+v", host)
	}
	// The sidecar dials the configured server origin, so on Linux it needs the
	// same host-gateway mapping the node carries; the argv builder mirrors it.
	if !reflect.DeepEqual(host.ExtraHosts, []string{"host.docker.internal:host-gateway"}) {
		t.Fatalf("sidecar host gateway = %v, want the node's Linux mapping", host.ExtraHosts)
	}
	if !strings.Contains(joined, "--add-host host.docker.internal:host-gateway") {
		t.Fatalf("sidecar argv missing host gateway: %v", args)
	}
	for _, entry := range spec.Env {
		if strings.Contains(entry, "API_KEY") || strings.Contains(entry, "ENROLLMENT") {
			t.Fatalf("sidecar env carries a credential: %v", spec.Env)
		}
	}
}

// TestEgressEnvMatchesToleratesImagePATH pins the real-engine defect Task 7
// found: the daemon merges every image's default PATH into the container env, so
// an exact env comparison rejected every real egress sidecar. The owned proxy
// variables remain exact and every other key is still drift.
func TestEgressEnvMatchesToleratesImagePATH(t *testing.T) {
	owned := []string{"MULTICA_EGRESS_SERVER_ORIGIN=https://multica.test", "MULTICA_EGRESS_ALLOWED_HOSTS=", "MULTICA_EGRESS_PINS="}
	basePATH := "PATH=" + defaultImagePATH
	cases := []struct {
		name   string
		actual []string
		want   bool
	}{
		{"owned only", owned, true},
		{"owned plus image path", append(append([]string{}, owned...), basePATH), true},
		{"changed owned value", []string{"MULTICA_EGRESS_SERVER_ORIGIN=https://evil.test", "MULTICA_EGRESS_ALLOWED_HOSTS="}, false},
		{"extra credential", append(append([]string{}, owned...), "ARK_API_KEY=secret"), false},
		{"missing owned", []string{basePATH}, false},
		{"duplicate key", append(append([]string{}, owned...), basePATH, basePATH), false},
	}
	for _, tc := range cases {
		if got := egressEnvMatches(tc.actual, owned); got != tc.want {
			t.Fatalf("%s: egressEnvMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClaudeProfileEnvUnchanged(t *testing.T) {
	p := &Provider{cfg: fixtureConfig()}
	n := fixtureNode()
	if got := p.nodeEnv(n); !reflect.DeepEqual(got, []string{"HOME=" + model.NodeHome, "FLEET_NODE_MAX_RUNS=1"}) {
		t.Fatalf("claude env changed: %v", got)
	}
	if inspectEnvironment([]string{"HTTP_PROXY=" + model.AuroraEgressProxyEndpoint}, 0, nil) {
		t.Fatal("claude profile accepted a proxy variable")
	}
	if inspectEnvironment([]string{"MULTICA_MANAGED=1"}, 0, nil) {
		t.Fatal("claude profile accepted managed env")
	}
	// Build a valid Claude snapshot exactly as the builder does and adopt it.
	h := NodeHostConfig(n.Resources, true, nil)
	h.NetworkMode = "node-net"
	r := container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "cid", HostConfig: &h}, Config: &container.Config{Image: n.Image, User: "10001:10001", Env: []string{"HOME=/data/home", "FLEET_NODE_MAX_RUNS=1"}, Entrypoint: []string{"/usr/local/bin/fleet-node"}, Cmd: []string{"run"}}, NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"node-net": {}}}, Mounts: []container.MountPoint{{Type: mount.TypeVolume, Name: n.DataVolume, Destination: model.DataMount, RW: true}, {Type: mount.TypeVolume, Name: n.SecretsVolume, Destination: "/secrets", RW: false}}}
	if err := validateNodeInspection(r, n, "node-net", fixtureConfig()); err != nil {
		t.Fatalf("claude snapshot rejected: %v", err)
	}
}

// TestEgressSidecarCarriesConfiguredPins proves the built sidecar spec and CLI
// argv carry the deterministic owned pins value, so the sidecar can dial the
// pinned public addresses instead of resolving.
func TestEgressSidecarCarriesConfiguredPins(t *testing.T) {
	cfg := auroraConfig()
	cfg.Aurora.EgressPins = map[string][]string{
		"ark.cn-beijing.volces.com": {"180.184.47.154"},
		"api.anthropic.com":         {"160.79.104.10"},
	}
	n := auroraNode()
	proxyName := New(fakeCalls{}, cfg).egressName(n)
	want := egressPinsEn + "=api.anthropic.com=160.79.104.10;ark.cn-beijing.volces.com=180.184.47.154"
	spec, _, err := egressProxySpec(cfg, n, proxyName, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range spec.Env {
		if entry == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("sidecar env = %v, want %q", spec.Env, want)
	}
	workspace := New(fakeCalls{}, cfg).workspaceNetwork(n).Name
	args, err := EgressProxyArgs(cfg, proxyName, workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, want) {
		t.Fatalf("sidecar argv missing pins: %v", args)
	}
}

// TestEgressEnvMatchesOwnsPins proves the pins variable is an owned sidecar value
// exactly like the allowlist: a mutated value or a sidecar that omits it is
// adoption drift, while the image's default PATH is still tolerated.
func TestEgressEnvMatchesOwnsPins(t *testing.T) {
	owned := []string{
		"MULTICA_EGRESS_SERVER_ORIGIN=https://multica.test",
		"MULTICA_EGRESS_ALLOWED_HOSTS=",
		egressPinsEn + "=ark.cn-beijing.volces.com=180.184.47.154",
	}
	basePATH := "PATH=" + defaultImagePATH
	cases := []struct {
		name   string
		actual []string
		want   bool
	}{
		{"owned only", owned, true},
		{"owned plus image path", append(append([]string{}, owned...), basePATH), true},
		{"mutated pins", []string{"MULTICA_EGRESS_SERVER_ORIGIN=https://multica.test", "MULTICA_EGRESS_ALLOWED_HOSTS=", egressPinsEn + "=ark.cn-beijing.volces.com=1.2.3.4"}, false},
		{"missing pins", []string{"MULTICA_EGRESS_SERVER_ORIGIN=https://multica.test", "MULTICA_EGRESS_ALLOWED_HOSTS="}, false},
		{"extra pins key", append(append([]string{}, owned...), "MULTICA_EGRESS_PINS_EXTRA=x"), false},
	}
	for _, tc := range cases {
		if got := egressEnvMatches(tc.actual, owned); got != tc.want {
			t.Fatalf("%s: egressEnvMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEgressSidecarHostGateway pins the Linux egress fix in the spec and the
// argv builder: the sidecar carries the same host-gateway mapping the node
// carries, and emits none when the flag is off, mirroring NodeHostConfig.
func TestEgressSidecarHostGateway(t *testing.T) {
	cfg := auroraConfig()
	n := auroraNode()
	proxyName := New(fakeCalls{}, cfg).egressName(n)
	workspace := New(fakeCalls{}, cfg).workspaceNetwork(n).Name
	want := []string{"host.docker.internal:host-gateway"}

	_, host, err := egressProxySpec(cfg, n, proxyName, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(host.ExtraHosts, want) {
		t.Fatalf("requested sidecar ExtraHosts = %v, want %v", host.ExtraHosts, want)
	}
	args, err := EgressProxyArgs(cfg, proxyName, workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostGatewayArgs(args); !reflect.DeepEqual(got, []string{"--add-host", want[0]}) {
		t.Fatalf("sidecar argv host gateway = %v, want %v", got, want)
	}

	_, off, err := egressProxySpec(cfg, n, proxyName, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(off.ExtraHosts) != 0 {
		t.Fatalf("unrequested sidecar gateway = %v", off.ExtraHosts)
	}
	offArgs, err := EgressProxyArgs(cfg, proxyName, workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := hostGatewayArgs(offArgs); len(got) != 0 {
		t.Fatalf("argv emitted an unrequested gateway: %v", got)
	}
}

// hostGatewayArgs returns the --add-host pairs in an argv, if any.
func hostGatewayArgs(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--add-host" {
			out = append(out, args[i], args[i+1])
		}
	}
	return out
}

// TestValidateEgressSidecarHostGateway pins the adoption authority exactly:
// a sidecar carrying the node's host-gateway mapping is adopted, while one
// missing it, carrying a different target, or carrying an extra mapping is
// rejected.
func TestValidateEgressSidecarHostGateway(t *testing.T) {
	cfg := auroraConfig()
	n := auroraNode()
	proxyName := New(fakeCalls{}, cfg).egressName(n)
	want, wantHost, err := egressProxySpec(cfg, n, proxyName, true)
	if err != nil {
		t.Fatal(err)
	}
	base := func() container.InspectResponse {
		host := wantHost
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "egress-id", HostConfig: &host},
			Config:            &container.Config{Image: want.Image, User: want.User, Labels: want.Labels, Env: append([]string(nil), want.Env...)},
		}
	}
	if err := validateEgressSidecar(cfg, n, proxyName, base()); err != nil {
		t.Fatalf("exact host gateway rejected: %v", err)
	}
	missing := base()
	missing.HostConfig.ExtraHosts = nil
	if err := validateEgressSidecar(cfg, n, proxyName, missing); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("sidecar missing host gateway accepted: %v", err)
	}
	wrong := base()
	wrong.HostConfig.ExtraHosts = []string{"host.docker.internal:1.2.3.4"}
	if err := validateEgressSidecar(cfg, n, proxyName, wrong); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("sidecar with a different mapping accepted: %v", err)
	}
	extra := base()
	extra.HostConfig.ExtraHosts = []string{"host.docker.internal:host-gateway", "other:1.2.3.4"}
	if err := validateEgressSidecar(cfg, n, proxyName, extra); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("sidecar with an extra mapping accepted: %v", err)
	}
}
