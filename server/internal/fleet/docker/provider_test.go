package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

func TestBuildNodeSpecSetsIdentityLabelsAndResources(t *testing.T) {
	spec := BuildNodeSpec("node-1", "wt-a", "n1", "img:test", "vol-data", "vol-secrets", "net-a", 4096, 2, 256)

	if spec.NodeID != "node-1" || spec.Namespace != "wt-a" || spec.Name != "n1" {
		t.Fatalf("identity: %+v", spec)
	}
	if spec.Image != "img:test" {
		t.Fatalf("image: %q", spec.Image)
	}
	if spec.DataVolume != "vol-data" || spec.SecretsVolume != "vol-secrets" || spec.Network != "net-a" {
		t.Fatalf("storage/network: %+v", spec)
	}
	if spec.MemoryMB != 4096 || spec.CPU != 2 || spec.PidsLimit != 256 {
		t.Fatalf("resources: %+v", spec)
	}
	wantLabels := map[string]string{
		"multica.fleet.namespace": "wt-a",
		"multica.fleet.node":      "node-1",
		"multica.fleet.role":      "node",
	}
	if len(spec.Labels) != len(wantLabels) {
		t.Fatalf("labels: %+v", spec.Labels)
	}
	for key, want := range wantLabels {
		if spec.Labels[key] != want {
			t.Fatalf("label %s = %q, want %q", key, spec.Labels[key], want)
		}
	}
	if len(spec.Env) != 0 {
		t.Fatalf("secrets must not be in Config.Env: %v", spec.Env)
	}
}

func TestBuildNodeSpecLabelsAreIndependent(t *testing.T) {
	first := BuildNodeSpec("node-1", "wt-a", "n1", "img:test", "d1", "s1", "net-a", 2048, 1, 64)
	first.Labels["multica.fleet.node"] = "mutated"

	second := BuildNodeSpec("node-2", "wt-a", "n2", "img:test", "d2", "s2", "net-a", 2048, 1, 64)
	if second.Labels["multica.fleet.node"] != "node-2" {
		t.Fatalf("labels shared between specs: %+v", second.Labels)
	}
}

func TestValidateNodeEnvAllowsExplicitPlatformVariables(t *testing.T) {
	allowed := []string{
		"MULTICA_SERVER_URL=http://host.docker.internal:8080",
		"MULTICA_DAEMON_ID=daemon-abc",
		"MULTICA_DAEMON_DEVICE_NAME=node-n1",
		"MULTICA_WORKSPACES_ROOT=/home/multica/workspaces",
		"MULTICA_DAEMON_AUTO_UPDATE=false",
		"MULTICA_DAEMON_AUTO_RELOAD=false",
		"HOME=/home/multica",
	}
	if err := validateNodeEnv(allowed); err != nil {
		t.Fatalf("allowed env rejected: %v", err)
	}
}

func TestValidateNodeEnvRejectsForbiddenVariables(t *testing.T) {
	cases := [][]string{
		{"MULTICA_TOKEN=x"},
		{"MULTICA_API_KEY=x"},
		{"ANTHROPIC_API_KEY=sk-x"},
		{"ANTHROPIC_AUTH_TOKEN=x"},
		{"OPENAI_API_KEY=x"},
		{"PATH=/evil"},
		{"LD_PRELOAD=/evil.so"},
		{"HOME=/home/multica", "AWS_SECRET_ACCESS_KEY=x"},
		{"HOME=/home/multica", "HOME=/tmp"},
		{"NOEQUALS"},
		{"=empty-key"},
	}
	for _, env := range cases {
		if err := validateNodeEnv(env); err == nil {
			t.Fatalf("expected rejection for %v", env)
		}
	}
}

func TestParseHealthReport(t *testing.T) {
	body := []byte(`{"status":"running","active_task_count":1,"running_task_count":1,"resource_wait_task_count":0,"pending_terminal_report_count":2,"agents":["claude"]}`)
	report, err := ParseHealthReport(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if report.Status != "running" || report.ActiveTaskCount != 1 || report.RunningTaskCount != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.ResourceWaitTaskCount != 0 || report.PendingTerminalReportCount != 2 {
		t.Fatalf("unexpected counts: %+v", report)
	}
	if len(report.Agents) != 1 || report.Agents[0] != "claude" {
		t.Fatalf("agents: %+v", report.Agents)
	}
}

func TestParseHealthReportRejectsMalformedBody(t *testing.T) {
	if _, err := ParseHealthReport([]byte("not-json")); err == nil {
		t.Fatal("expected malformed body to fail")
	}
	if _, err := ParseHealthReport(nil); err == nil {
		t.Fatal("expected empty body to fail")
	}
}

func TestValidateNodeSpec(t *testing.T) {
	valid := BuildNodeSpec("node-1", "wt-a", "n1", "img:test", "vol-data", "vol-secrets", "net-a", 4096, 2, 256)
	if err := validateNodeSpec(valid); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	atLimit := BuildNodeSpec("node-1", "wt-a", "n1", "img:test", "vol-data", "vol-secrets", "net-a", maxNodeMemoryMB, 2, 256)
	if err := validateNodeSpec(atLimit); err != nil {
		t.Fatalf("memory at limit rejected: %v", err)
	}

	cases := map[string]func(*NodeSpec){
		"empty image":         func(s *NodeSpec) { s.Image = "" },
		"empty name":          func(s *NodeSpec) { s.Name = "" },
		"empty node id":       func(s *NodeSpec) { s.NodeID = "" },
		"empty namespace":     func(s *NodeSpec) { s.Namespace = "" },
		"empty data volume":   func(s *NodeSpec) { s.DataVolume = "" },
		"empty secrets":       func(s *NodeSpec) { s.SecretsVolume = "" },
		"shared volume":       func(s *NodeSpec) { s.SecretsVolume = s.DataVolume },
		"empty network":       func(s *NodeSpec) { s.Network = "" },
		"host network":        func(s *NodeSpec) { s.Network = "host" },
		"none network":        func(s *NodeSpec) { s.Network = "none" },
		"default network":     func(s *NodeSpec) { s.Network = "bridge" },
		"shared network":      func(s *NodeSpec) { s.Network = "container:abc" },
		"zero memory":         func(s *NodeSpec) { s.MemoryMB = 0 },
		"negative memory":     func(s *NodeSpec) { s.MemoryMB = -1 },
		"oversized memory":    func(s *NodeSpec) { s.MemoryMB = maxNodeMemoryMB + 1 },
		"zero cpu":            func(s *NodeSpec) { s.CPU = 0 },
		"negative cpu":        func(s *NodeSpec) { s.CPU = -1 },
		"nan cpu":             func(s *NodeSpec) { s.CPU = math.NaN() },
		"positive inf cpu":    func(s *NodeSpec) { s.CPU = math.Inf(1) },
		"negative inf cpu":    func(s *NodeSpec) { s.CPU = math.Inf(-1) },
		"zero pids":           func(s *NodeSpec) { s.PidsLimit = 0 },
		"missing role label":  func(s *NodeSpec) { delete(s.Labels, "multica.fleet.role") },
		"wrong node label":    func(s *NodeSpec) { s.Labels["multica.fleet.node"] = "other" },
		"wrong ns label":      func(s *NodeSpec) { s.Labels["multica.fleet.namespace"] = "other" },
		"forbidden env":       func(s *NodeSpec) { s.Env = []string{"MULTICA_TOKEN=x"} },
		"malformed env entry": func(s *NodeSpec) { s.Env = []string{"HOME"} },
	}
	for name, mutate := range cases {
		spec := BuildNodeSpec("node-1", "wt-a", "n1", "img:test", "vol-data", "vol-secrets", "net-a", 4096, 2, 256)
		mutate(&spec)
		if err := validateNodeSpec(spec); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

// The Provider interface is consumed by the preflight code, which needs to
// distinguish a missing image from other Docker failures before creating
// a container.
func TestProviderExposesInspectImage(t *testing.T) {
	var _ = func(p Provider) error {
		return p.InspectImage(context.Background(), "img:test")
	}
}

// PrepareVolumes needs a live daemon, so only the contract is asserted here.
// Task 12's opt-in Docker integration test owns the real seeding round-trip.
func TestProviderExposesPrepareVolumesAndClose(t *testing.T) {
	var _ = func(p Provider) error {
		return p.PrepareVolumes(context.Background(), "vol-data", "vol-secrets", "img:test", map[string][]byte{"bootstrap.env": []byte("X=1")})
	}
	var _ = func(p Provider) error { return p.Close() }
}

func TestBuildSecretsTarWritesSortedOwnerOnlyFiles(t *testing.T) {
	files := map[string][]byte{
		"zeta.env":  []byte("Z=1\n"),
		"alpha.env": []byte("A=1\n"),
		"mid.json":  []byte("{}\n"),
	}
	archive, err := buildSecretsTar(files)
	if err != nil {
		t.Fatalf("build secrets tar: %v", err)
	}

	tr := tar.NewReader(bytes.NewReader(archive))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		names = append(names, hdr.Name)
		if hdr.Typeflag != tar.TypeReg {
			t.Fatalf("%s: typeflag = %v, want regular file", hdr.Name, hdr.Typeflag)
		}
		if hdr.Mode != 0o600 {
			t.Fatalf("%s: mode = %#o, want 0600", hdr.Name, hdr.Mode)
		}
		if hdr.Uid != nodeUID || hdr.Gid != nodeGID {
			t.Fatalf("%s: owner = %d:%d, want %d:%d", hdr.Name, hdr.Uid, hdr.Gid, nodeUID, nodeGID)
		}
		if strings.ContainsAny(hdr.Name, "/\\") {
			t.Fatalf("%s: archive entries must be flat", hdr.Name)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("%s: read body: %v", hdr.Name, err)
		}
		if !bytes.Equal(body, files[hdr.Name]) {
			t.Fatalf("%s: body = %q, want %q", hdr.Name, body, files[hdr.Name])
		}
	}
	want := []string{"alpha.env", "mid.json", "zeta.env"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
}

func TestBuildSecretsTarRejectsUnsafeNames(t *testing.T) {
	cases := [][]string{
		{"../escape.env"},
		{"dir/file.env"},
		{".."},
		{""},
		{"a\\b"},
		{"a..b"},
	}
	for _, names := range cases {
		files := make(map[string][]byte, len(names))
		for _, name := range names {
			files[name] = []byte("x")
		}
		if _, err := buildSecretsTar(files); err == nil {
			t.Fatalf("expected rejection for %q", names)
		}
	}
	if _, err := buildSecretsTar(nil); err == nil {
		t.Fatal("expected empty file set to fail")
	}
}

// The runtime image's entrypoint starts the node daemon and never exits, so a
// helper that inherits it would run the daemon as root forever and ContainerWait
// would never return. An empty slice must therefore stay non-nil: the daemon's
// config merge only skips the image entrypoint when the slice is non-nil.
func TestBuildSeedContainerConfigClearsImageEntrypoint(t *testing.T) {
	config := buildSeedContainerConfig("img:test")
	if config.Entrypoint == nil {
		t.Fatal("entrypoint must be an empty slice, not nil: nil inherits the image entrypoint")
	}
	if len(config.Entrypoint) != 0 {
		t.Fatalf("entrypoint = %v, want empty", config.Entrypoint)
	}
	if config.Image != "img:test" {
		t.Fatalf("image = %q", config.Image)
	}
	if config.User != "0:0" {
		t.Fatalf("user = %q, want root for chown", config.User)
	}
	if config.WorkingDir != "/" {
		t.Fatalf("working dir = %q, want /: the helper must not depend on the image WORKDIR", config.WorkingDir)
	}
	// Locking the command also locks the helper's mount paths: it sees the data
	// volume at seedDataMountPath and the secrets volume at secretsMountPath.
	wantCmd := []string{"sh", "-c", "chown -R 1000:1000 /data /run/secrets/runtime && chmod -R go-rwx /run/secrets/runtime"}
	// config.Cmd is a strslice.StrSlice, so reflect.DeepEqual would report a
	// difference on the named slice type alone; compare the elements instead.
	if !slices.Equal(config.Cmd, wantCmd) {
		t.Fatalf("cmd = %q, want %q", config.Cmd, wantCmd)
	}
	if len(config.Labels) != 1 || config.Labels[LabelRole] != RoleSeed {
		t.Fatalf("labels = %v, want only %s=%s", config.Labels, LabelRole, RoleSeed)
	}
}

func TestBuildSeedHostConfigMountsBothVolumesReadWriteWithoutNetwork(t *testing.T) {
	hostConfig := buildSeedHostConfig("vol-data", "vol-secrets")
	if hostConfig.NetworkMode != container.NetworkMode("none") {
		t.Fatalf("network mode = %q, want none", hostConfig.NetworkMode)
	}
	if hostConfig.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Fatalf("restart policy = %q, want disabled", hostConfig.RestartPolicy.Name)
	}
	if hostConfig.Privileged {
		t.Fatal("helper must not be privileged")
	}
	want := []mount.Mount{
		{Type: mount.TypeVolume, Source: "vol-data", Target: seedDataMountPath},
		{Type: mount.TypeVolume, Source: "vol-secrets", Target: secretsMountPath},
	}
	if len(hostConfig.Mounts) != len(want) {
		t.Fatalf("mounts = %+v, want %+v", hostConfig.Mounts, want)
	}
	for i, got := range hostConfig.Mounts {
		if got.Type != want[i].Type || got.Source != want[i].Source || got.Target != want[i].Target {
			t.Fatalf("mount %d = %+v, want %+v", i, got, want[i])
		}
		if got.ReadOnly {
			t.Fatalf("mount %d must be writable for the seed command", i)
		}
	}
}

func TestSeedWaitErrorDistinguishesTimeoutFromCancellation(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err := seedWaitError(canceledCtx, "helper-1", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation not preserved: %v", err)
	}

	err = seedWaitError(context.Background(), "helper-1", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("helper timeout must be a deadline error: %v", err)
	}
	if !strings.Contains(err.Error(), seedTimeout.String()) {
		t.Fatalf("helper timeout error must report %s: %v", seedTimeout, err)
	}

	// The SDK can surface a transport error when the wait request is canceled;
	// the helper timeout must still be recognizable while keeping the cause.
	err = seedWaitError(context.Background(), "helper-1", errors.New("connection reset"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("helper timeout with cause must remain a deadline error: %v", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("helper timeout must keep the underlying cause: %v", err)
	}
}
