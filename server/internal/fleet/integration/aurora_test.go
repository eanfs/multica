//go:build dockerintegration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/multica-ai/multica/server/internal/fleet/docker"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// This file is the Task 7 opt-in proof: one Aurora generation completed on the
// real local Docker Fleet with a fake pipeline (no model, no provider account),
// plus the real-engine idempotency/crash-replay/live-egress evidence Task 5's
// review carried forward, plus the real MCP broker starting under the injected
// config and advertising mcp__aurora__* tool identifiers. Every subtest is under
// the dockerintegration build tag and checks MULTICA_RUN_DOCKER_INTEGRATION=1
// before any Docker client, CLI or database lookup, so a default run touches
// nothing.

// TestAuroraRoundTrip is the single gated entry point. The gate is the very
// first action.
func TestAuroraRoundTrip(t *testing.T) {
	if os.Getenv("MULTICA_RUN_DOCKER_INTEGRATION") != "1" {
		t.Skip("Docker opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	t.Run("ApiGenerationRoundTrip", func(t *testing.T) {
		if err := RoundTripAuroraGeneration(ctx, t); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("RealEngineConnectNetworkIsIdempotent", func(t *testing.T) {
		testConnectNetworkIdempotentRealEngine(ctx, t)
	})
	// The crash-replay of ensureEgress over a running sidecar is proven in the
	// docker package (TestAuroraRoundTripEgressReplay), where the unexported
	// helper can be driven without Provider.Ensure's node admission. See the Task
	// 7 report: real-node admission is separately blocked by the seccomp path the
	// SDK sends verbatim to the daemon.
	t.Run("RealEngineLiveEgress", func(t *testing.T) {
		testLiveEgressRealEngine(ctx, t)
	})
	t.Run("RealBrokerAllowedTools", func(t *testing.T) {
		testRealBrokerAllowedTools(ctx, t)
	})
}

// RoundTripAuroraGeneration drives the real API/Fleet/Docker path from a
// generation request to a settled, asset-backed generation. It is input-gated:
// the managed environment must supply the API, the workspace, the database and
// the digest-pinned dual-contract sandbox node image. A SKIP here is not a pass.
func RoundTripAuroraGeneration(ctx context.Context, t *testing.T) error {
	t.Helper()
	env, err := auroraRoundTripInputs(t)
	if err != nil {
		return nil // the helper already skipped with the missing input named
	}

	api := &fleetAPI{base: env.apiURL, token: env.token, workspaceID: env.workspaceID, http: &http.Client{Timeout: 30 * time.Second}}

	skillID := "xhs-image"
	prompt := "docker round trip poster"
	var created struct {
		Generation struct {
			ID              string `json:"id"`
			Status          string `json:"status"`
			CreditsReserved int64  `json:"creditsReserved"`
		} `json:"generation"`
	}
	if err := api.call(ctx, http.MethodPost, "/api/aurora/generations", "", map[string]any{
		"skillId": skillID,
		"prompt":  prompt,
	}, &created); err != nil {
		return fmt.Errorf("API create generation: %w", err)
	}
	if created.Generation.ID == "" {
		return fmt.Errorf("API create generation returned no id")
	}
	generationID := created.Generation.ID

	// The generation row is the authoritative terminal record. Poll the detail
	// endpoint, which derives in-flight status from the backing task.
	deadline := time.Now().Add(20 * time.Minute)
	for {
		var detail struct {
			Generation struct {
				ID             string `json:"id"`
				Status         string `json:"status"`
				CreditsCharged int64  `json:"creditsCharged"`
				Assets         []struct {
					ID   string `json:"id"`
					Kind string `json:"kind"`
				} `json:"assets"`
			} `json:"generation"`
		}
		if err := api.call(ctx, http.MethodGet, "/api/aurora/generations/"+generationID, "", nil, &detail); err != nil {
			return fmt.Errorf("API get generation: %w", err)
		}
		switch detail.Generation.Status {
		case "completed":
			if detail.Generation.CreditsCharged <= 0 {
				return fmt.Errorf("generation %s completed without credits_charged (reserved=%d, charged=%d)", generationID, created.Generation.CreditsReserved, detail.Generation.CreditsCharged)
			}
			if len(detail.Generation.Assets) == 0 {
				return fmt.Errorf("generation %s completed with no aurora_asset rows", generationID)
			}
			return nil
		case "failed":
			return fmt.Errorf("generation %s failed before producing an artifact", generationID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("generation %s did not settle before the deadline (status %q)", generationID, detail.Generation.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// auroraRoundTripInputs validates the explicit Aurora inputs before any side
// effect. It reuses the shared Fleet harness identity and adds the sandbox
// image the fake pipeline runs in.
func auroraRoundTripInputs(t *testing.T) (roundTripEnv, error) {
	t.Helper()
	env := roundTripEnv{
		apiURL:      strings.TrimRight(os.Getenv("MULTICA_FLEET_API_URL"), "/"),
		token:       os.Getenv("MULTICA_FLEET_API_TOKEN"),
		workspaceID: os.Getenv("MULTICA_FLEET_WORKSPACE_ID"),
		image:       os.Getenv("MULTICA_AURORA_TEST_NODE_IMAGE"),
	}
	if env.image == "" {
		env.image = os.Getenv("MULTICA_FLEET_TEST_IMAGE")
	}
	for _, missing := range []struct{ name, value, why string }{
		{"MULTICA_FLEET_API_URL", env.apiURL, "the running managed API"},
		{"MULTICA_FLEET_API_TOKEN", env.token, "an authenticated workspace caller"},
		{"MULTICA_FLEET_WORKSPACE_ID", env.workspaceID, "the workspace under test"},
		{"MULTICA_AURORA_TEST_NODE_IMAGE", env.image, "the digest-pinned dual-contract Aurora sandbox node image"},
	} {
		if missing.value == "" {
			t.Skipf("%s required for the Aurora round trip (%s)", missing.name, missing.why)
			return env, errors.New("missing input")
		}
	}
	if !approvedNodeImage.MatchString(env.image) {
		t.Skip("MULTICA_AURORA_TEST_NODE_IMAGE must be digest pinned")
		return env, errors.New("bad image")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required for the Aurora round trip")
		return env, errors.New("missing database")
	}
	return env, nil
}

// --- real-engine evidence ---------------------------------------------------

type auroraEngineEnv struct {
	cli       *client.Client
	engine    docker.Engine
	cfg       model.Config
	namespace string
	fleetID   string
	uplink    string
}

// newAuroraEngineEnv builds the real Docker client, an Aurora provider config,
// and a temp seccomp profile. It registers cleanup that removes only the
// resources this test labelled; it never prunes.
func newAuroraEngineEnv(ctx context.Context, t *testing.T) *auroraEngineEnv {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("Docker engine unavailable: %v", err)
	}
	if _, err := cli.Ping(ctx); err != nil {
		cli.Close()
		t.Skipf("Docker engine unavailable: %v", err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	ns := "aurora-it-" + suffix
	fleetID := "aurora-it"
	env := &auroraEngineEnv{
		cli:       cli,
		engine:    docker.NewEngine(cli),
		namespace: ns,
		fleetID:   fleetID,
		uplink:    "aurora-it-uplink-" + suffix,
	}

	nodeImage, err := auroraTestNodeImage(ctx, cli)
	if err != nil {
		cli.Close()
		t.Skipf("Aurora test node image unavailable: %v", err)
	}
	proxyImage, err := resolveImageDigestRef(ctx, cli, firstNonEmpty(os.Getenv("MULTICA_AURORA_TEST_PROXY_IMAGE"), "multica-aurora-egress-fixture:arm64"))
	if err != nil {
		cli.Close()
		t.Skipf("Aurora egress fixture image unavailable: %v", err)
	}
	// The Docker daemon reads a seccomp=/<path> option itself, and Docker Desktop
	// only shares the host workspace tree into its VM, so the profile must live
	// under the checkout, not under the host temp directory.
	seccompDir, err := os.MkdirTemp(".", ".aurora-it-seccomp-")
	if err != nil {
		cli.Close()
		t.Fatalf("create seccomp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(seccompDir) })
	absSeccompDir, err := filepath.Abs(seccompDir)
	if err != nil {
		cli.Close()
		t.Fatalf("resolve seccomp dir: %v", err)
	}
	seccompPath := filepath.Join(absSeccompDir, "seccomp.json")
	if err := os.WriteFile(seccompPath, []byte(`{"defaultAction":"SCMP_ACT_ALLOW","architectures":["SCMP_ARCH_AARCH64","SCMP_ARCH_X86_64"],"syscalls":[]}`), 0o600); err != nil {
		cli.Close()
		t.Fatalf("write seccomp profile: %v", err)
	}

	// A real server origin is required by the profile and by the egress policy.
	// The proxy authorizes exactly this origin, so the live-egress test can use a
	// loopback control-plane stand-in instead of a public host.
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "aurora-it-control-ok")
	}))
	t.Cleanup(control.Close)
	// The sandbox reaches the control plane by name from inside the VM, so the
	// profile origin uses the Docker Desktop host alias while the test listener
	// stays on the host loopback.
	serverOrigin := strings.Replace(control.URL, "127.0.0.1", "host.docker.internal", 1)

	env.cfg = model.Config{
		Namespace: ns,
		FleetID:   fleetID,
		Image:     nodeImage,
		APIURL:    control.URL,
		MaxNodes:  2,
		Specs:     map[string]model.Spec{"sandbox": {CPUs: 2, MemoryBytes: 4 << 30, Pids: 256, MaxRuns: 1}},
		Aurora: &model.AuroraConfig{
			ServerURL:           serverOrigin,
			ProxyImage:          proxyImage,
			SeccompProfile:      seccompPath,
			AppArmorProfile:     "multica-aurora-sandbox",
			ProviderSecretFiles: map[string]string{},
			ReadonlyRootfs:      true,
			UplinkNetwork:       env.uplink,
		},
	}
	if err := env.cfg.Aurora.Validate(); err != nil {
		cli.Close()
		t.Fatalf("test Aurora config invalid: %v", err)
	}

	// The uplink network is operator-provided, so the test creates it before the
	// provider asks for the sidecar.
	if _, err := cli.NetworkCreate(ctx, env.uplink, network.CreateOptions{Driver: "bridge", Labels: map[string]string{"aurora.it": ns}}); err != nil {
		cli.Close()
		t.Fatalf("create uplink network: %v", err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		removeAuroraTestResources(t, cleanup, cli, ns, env.uplink)
		cli.Close()
	})
	return env
}

// testConnectNetworkIdempotentRealEngine proves, against the real dockerd, that
// a second ConnectNetwork on an already-attached container is nil and that a
// distinct failure (a network that does not exist) still fails.
func testConnectNetworkIdempotentRealEngine(ctx context.Context, t *testing.T) {
	env := newAuroraEngineEnv(ctx, t)

	wsName := "aurora-it-ws-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if _, err := env.cli.NetworkCreate(ctx, wsName, network.CreateOptions{Driver: "bridge", Internal: true, Labels: map[string]string{"aurora.it": env.namespace}}); err != nil {
		t.Fatalf("create workspace network: %v", err)
	}
	created, err := env.cli.ContainerCreate(ctx, &container.Config{
		Image:      env.cfg.Image,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", "sleep 3600"},
		Labels:     map[string]string{"aurora.it": env.namespace},
	}, &container.HostConfig{}, nil, nil, "aurora-it-attach-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("create probe container: %v", err)
	}
	if err := env.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start probe container: %v", err)
	}

	if err := env.engine.ConnectNetwork(ctx, wsName, created.ID, []string{"egress"}); err != nil {
		t.Fatalf("first ConnectNetwork: %v", err)
	}
	if err := env.engine.ConnectNetwork(ctx, wsName, created.ID, []string{"egress"}); err != nil {
		t.Fatalf("second ConnectNetwork on an already-connected container: %v", err)
	}
	if err := env.engine.ConnectNetwork(ctx, wsName+"-missing", created.ID, []string{"egress"}); err == nil {
		t.Fatal("ConnectNetwork on a missing network unexpectedly succeeded")
	}
}

// testLiveEgressRealEngine runs the real egress sidecar image on an internal
// workspace network and proves a node resolves the fixed "egress" alias, that
// the proxy reaches the configured origin, and that an unlisted host is refused.
func testLiveEgressRealEngine(ctx context.Context, t *testing.T) {
	env := newAuroraEngineEnv(ctx, t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	wsName := "aurora-it-ws-" + suffix
	if _, err := env.cli.NetworkCreate(ctx, wsName, network.CreateOptions{Driver: "bridge", Internal: true, Labels: map[string]string{"aurora.it": env.namespace}}); err != nil {
		t.Fatalf("create workspace network: %v", err)
	}
	proxyName := "aurora-it-egress-" + suffix
	proxyLabels := map[string]string{"aurora.it": env.namespace}
	proxy, err := env.cli.ContainerCreate(ctx, &container.Config{
		Image:  env.cfg.Aurora.ProxyImage,
		User:   "10001:10001",
		Env:    []string{"MULTICA_EGRESS_SERVER_ORIGIN=" + env.cfg.Aurora.ServerURL, "MULTICA_EGRESS_ALLOWED_HOSTS="},
		Labels: proxyLabels,
	}, &container.HostConfig{
		NetworkMode:    container.NetworkMode(env.uplink),
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
	}, nil, nil, proxyName)
	if err != nil {
		t.Fatalf("create egress sidecar: %v", err)
	}
	if err := env.cli.ContainerStart(ctx, proxy.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start egress sidecar: %v", err)
	}
	if err := env.engine.ConnectNetwork(ctx, wsName, proxy.ID, []string{"egress"}); err != nil {
		t.Fatalf("attach egress alias: %v", err)
	}

	nodeName := "aurora-it-node-" + suffix
	nodeImage, err := resolveImageDigestRef(ctx, env.cli, firstNonEmpty(os.Getenv("MULTICA_AURORA_TEST_EGRESS_NODE_IMAGE"), os.Getenv("MULTICA_FLEET_TEST_NODE_IMAGE"), "multica-aurora-sandbox-fleet-fixture:local"))
	if err != nil {
		t.Skipf("node image with a shell is unavailable: %v", err)
	}
	node, err := env.cli.ContainerCreate(ctx, &container.Config{
		Image:      nodeImage,
		User:       "10001:10001",
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", "sleep 3600"},
		Env:        []string{"HOME=/data/home", "HTTP_PROXY=" + model.AuroraEgressProxyEndpoint, "HTTPS_PROXY=" + model.AuroraEgressProxyEndpoint, "NO_PROXY=" + model.AuroraNoProxyValue},
		Labels:     proxyLabels,
	}, &container.HostConfig{NetworkMode: container.NetworkMode(wsName)}, nil, nil, nodeName)
	if err != nil {
		t.Fatalf("create node container: %v", err)
	}
	if err := env.cli.ContainerStart(ctx, node.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start node container: %v", err)
	}

	allowed := env.cfg.Aurora.ServerURL + "/aurora-it"
	allowedBody, err := execInContainer(ctx, env.cli, node.ID, proxyProbeCommand(allowed))
	if err != nil {
		t.Fatalf("allowed-host proxy request through egress: %v", err)
	}
	if !strings.Contains(allowedBody, "200") {
		t.Fatalf("allowed-host proxy response = %q, want HTTP 200", allowedBody)
	}
	refusedBody, err := execInContainer(ctx, env.cli, node.ID, proxyProbeCommand("http://example.com/"))
	if err != nil {
		t.Fatalf("unlisted-host proxy request: %v", err)
	}
	if !strings.Contains(refusedBody, "403") {
		t.Fatalf("unlisted-host proxy response = %q, want HTTP 403", refusedBody)
	}
	sidecar, err := env.cli.ContainerInspect(ctx, proxy.ID)
	if err != nil {
		t.Fatalf("inspect sidecar: %v", err)
	}
	if sidecar.State == nil || sidecar.State.Status != "running" {
		t.Fatalf("sidecar state = %+v, want running", sidecar.State)
	}
}

// proxyProbeCommand makes one plain-HTTP request through the fixed proxy. Node
// is the only HTTP client in the fixture image; curl's "-x" semantics are the
// same request line.
func proxyProbeCommand(target string) []string {
	script := `const http=require('http');const u=new URL(process.argv[1]);const req=http.request({host:'egress',port:3128,path:process.argv[1],method:'GET',headers:{host:u.host}},res=>{let d='';res.on('data',c=>d+=c);res.on('end',()=>{console.log(res.statusCode);});});req.on('error',e=>{console.error('ERR '+e.message);process.exit(3);});req.end();`
	return []string{"node", "-e", script, target}
}

// --- real broker acceptance -------------------------------------------------

var claudeMCPToolName = regexp.MustCompile(`^mcp__[A-Za-z0-9_-]+(?:__(?:[A-Za-z0-9_.-]+|[*]))?$`)

// testRealBrokerAllowedTools starts the real Aurora MCP broker from the checked
// out runtime under the injected environment and proves it advertises the nine
// aurora.* tools and that the daemon's mcp__aurora__<tool> identifiers are the
// broker's own tool names (Claude's validator accepts them).
func testRealBrokerAllowedTools(ctx context.Context, t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to run the real Aurora broker")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	entrypoint := filepath.Join(repoRoot, "deploy", "aurora-sandbox", "runtime", "src", "server.mjs")
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("real broker entrypoint unavailable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "deploy", "aurora-sandbox", "runtime", "node_modules")); err != nil {
		t.Skipf("real broker dependencies unavailable: %v", err)
	}

	dir := t.TempDir()
	inputRoot := filepath.Join(dir, "input")
	outputRoot := filepath.Join(dir, "output")
	secretsDir := filepath.Join(dir, "secrets")
	for _, d := range []string{inputRoot, outputRoot, secretsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	secretValues := map[string]string{"ark": "ark-fixture", "openai": "openai-fixture", "volc": "volc-fixture"}
	secretPaths := map[string]string{}
	for key, value := range secretValues {
		p := filepath.Join(secretsDir, key)
		if err := os.WriteFile(p, []byte(value), 0o400); err != nil {
			t.Fatalf("write secret %s: %v", key, err)
		}
		secretPaths[key] = p
	}
	tokenPath := filepath.Join(secretsDir, "task-token")
	if err := os.WriteFile(tokenPath, []byte("mtt-fixture-token"), 0o400); err != nil {
		t.Fatalf("write task token: %v", err)
	}
	serverOrigin := "http://127.0.0.1:1"
	contextPath := filepath.Join(dir, "task-context.json")
	contextJSON, _ := json.Marshal(map[string]any{
		"schema":          "com.multica.aurora.task-context",
		"version":         1,
		"task_id":         "44444444-4444-4444-8444-444444444444",
		"generation_id":   "55555555-5555-4555-8555-555555555555",
		"workspace_id":    "66666666-6666-4666-8666-666666666666",
		"skill_id":        "xhs-image",
		"prompt":          "broker acceptance",
		"attachments":     map[string]any{},
		"output_root":     outputRoot,
		"server_origin":   serverOrigin,
		"task_token_file": tokenPath,
	})
	if err := os.WriteFile(contextPath, contextJSON, 0o400); err != nil {
		t.Fatalf("write task context: %v", err)
	}

	cmd := exec.CommandContext(ctx, node, entrypoint)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"AURORA_SERVER_ORIGIN="+serverOrigin,
		"AURORA_TASK_CONTEXT_FILE="+contextPath,
		"AURORA_INPUT_ROOT="+inputRoot,
		"AURORA_OUTPUT_ROOT="+outputRoot,
		"AURORA_ARTIFACT_IMPORT_PATH=/api/agent/tasks/44444444-4444-4444-8444-444444444444/aurora-artifacts/import",
		"ARK_API_KEY_FILE="+secretPaths["ark"],
		"OPENAI_API_KEY_FILE="+secretPaths["openai"],
		"VOLC_ASR_API_KEY_FILE="+secretPaths["volc"],
		"AURORA_TASK_TOKEN_FILE="+tokenPath,
		"HOME="+dir,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("broker stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("broker stdout: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start real broker: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	})

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	write := func(line string) {
		if _, err := io.WriteString(stdin, line+string(byte(10))); err != nil {
			t.Fatalf("write broker request: %v (stderr: %s)", err, stderr.String())
		}
	}
	readResult := func(id int) map[string]json.RawMessage {
		for scanner.Scan() {
			var msg map[string]json.RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
				continue
			}
			var gotID int
			if raw, ok := msg["id"]; ok {
				_ = json.Unmarshal(raw, &gotID)
			}
			if gotID == id {
				return msg
			}
		}
		t.Fatalf("broker produced no response for request %d (stderr: %s)", id, stderr.String())
		return nil
	}

	write(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"aurora-it","version":"0"}}}`)
	init := readResult(1)
	if _, ok := init["result"]; !ok {
		t.Fatalf("broker initialize failed: %s (stderr: %s)", string(init["error"]), stderr.String())
	}
	write(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	write(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	list := readResult(2)

	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(list["result"], &tools); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	got := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	want := []string{
		"aurora.id_photo",
		"aurora.openai_image",
		"aurora.read_document",
		"aurora.render_resume",
		"aurora.render_video_captions",
		"aurora.seedance_generate",
		"aurora.seedream_generate",
		"aurora.volc_asr_transcribe",
		"aurora.write_text_artifact",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("broker tools = %v, want %v (stderr: %s)", got, want, stderr.String())
	}
	// The daemon emits --allowedTools identifiers as mcp__aurora__<tool>. Prove
	// they are exactly this broker's tool names under Claude's validator.
	for _, tool := range want {
		identifier := "mcp__aurora__" + tool
		if !claudeMCPToolName.MatchString(identifier) {
			t.Fatalf("identifier %q is not a valid Claude MCP tool name", identifier)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func resolveImageDigestRef(ctx context.Context, cli *client.Client, ref string) (string, error) {
	if strings.Contains(ref, "@sha256:") {
		if _, _, err := cli.ImageInspectWithRaw(ctx, ref); err != nil {
			return "", err
		}
		return ref, nil
	}
	insp, _, err := cli.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(insp.ID, "sha256:") {
		return "", fmt.Errorf("image %s has no immutable id", ref)
	}
	name := ref
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name = ref[:i]
	}
	return name + "@" + insp.ID, nil
}

const auroraNodeFixtureTag = "multica-aurora-it-node:local"
const auroraNodeFixtureDockerfile = "testdata/Dockerfile.aurora-node-fixture"

// auroraTestNodeImage resolves the digest-pinned node image the real-engine
// tests admit. MULTICA_AURORA_TEST_NODE_IMAGE always wins. Otherwise the test
// builds a throwaway fixture from the committed testdata Dockerfile: its
// fleet-node subcommands match the fixed contract and its only image env is
// PATH/HOME, so the provider's strict image inspection accepts it and
// `fleet-node bootstrap` accepts the Aurora installer. Nothing is pushed.
func auroraTestNodeImage(ctx context.Context, cli *client.Client) (string, error) {
	if explicit := os.Getenv("MULTICA_AURORA_TEST_NODE_IMAGE"); explicit != "" {
		return resolveImageDigestRef(ctx, cli, explicit)
	}
	if ref, err := resolveImageDigestRef(ctx, cli, auroraNodeFixtureTag); err == nil {
		return ref, nil
	}
	if _, _, err := cli.ImageInspectWithRaw(ctx, "alpine:3.20"); err != nil {
		return "", fmt.Errorf("alpine:3.20 base is required to build the Aurora node fixture: %w", err)
	}
	build := exec.CommandContext(ctx, "docker", "build", "-f", auroraNodeFixtureDockerfile, "-t", auroraNodeFixtureTag, filepath.Dir(auroraNodeFixtureDockerfile))
	if out, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build Aurora node fixture: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return resolveImageDigestRef(ctx, cli, auroraNodeFixtureTag)
}

func execInContainer(ctx context.Context, cli *client.Client, id string, cmd []string) (string, error) {
	exec, err := cli.ContainerExecCreate(ctx, id, container.ExecOptions{Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return "", err
	}
	resp, err := cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer resp.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Reader, 1<<20))
	inspected, err := cli.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return string(out), err
	}
	if inspected.ExitCode != 0 {
		return string(out), fmt.Errorf("exec %v exited %d: %s", cmd, inspected.ExitCode, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// removeAuroraTestResources removes only the resources this test labelled and
// the explicit uplink network. It never prunes.
func removeAuroraTestResources(t *testing.T, ctx context.Context, cli *client.Client, namespace, uplink string) {
	t.Helper()
	label := "multica.fleet.namespace=" + namespace
	f := filters.NewArgs(filters.Arg("label", label))
	contexts, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	containers, err := cli.ContainerList(contexts, container.ListOptions{All: true, Filters: f})
	if err == nil {
		for _, c := range containers {
			_ = cli.ContainerRemove(contexts, c.ID, container.RemoveOptions{Force: true})
		}
	}
	testFilter := filters.NewArgs(filters.Arg("label", "aurora.it="+namespace))
	containers, err = cli.ContainerList(contexts, container.ListOptions{All: true, Filters: testFilter})
	if err == nil {
		for _, c := range containers {
			_ = cli.ContainerRemove(contexts, c.ID, container.RemoveOptions{Force: true})
		}
	}
	for _, filter := range []filters.Args{f, testFilter} {
		vols, verr := cli.VolumeList(contexts, volume.ListOptions{Filters: filter})
		if verr == nil {
			for _, v := range vols.Volumes {
				_ = cli.VolumeRemove(contexts, v.Name, true)
			}
		}
		nets, nerr := cli.NetworkList(contexts, network.ListOptions{Filters: filter})
		if nerr == nil {
			for _, nw := range nets {
				_ = cli.NetworkRemove(contexts, nw.ID)
			}
		}
	}
	_ = cli.NetworkRemove(contexts, uplink)
	// Leave a receipt the report can quote: the owned inventory must be empty.
	remaining, _ := cli.ContainerList(contexts, container.ListOptions{All: true, Filters: f})
	if len(remaining) != 0 {
		t.Logf("WARNING: %d owned containers remained after cleanup", len(remaining))
	}
}
