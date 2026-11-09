package docker

import (
	"context"
	"testing"
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
		"zero cpu":            func(s *NodeSpec) { s.CPU = 0 },
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
