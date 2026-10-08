package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// Fixed UUIDs so the context file the broker validates is shape-correct.
const (
	auroraBrokerTestTaskID      = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	auroraBrokerTestGeneration  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	auroraBrokerTestWorkspaceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	auroraBrokerTestAgentID     = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	auroraBrokerTestSkill       = "text-image"
	auroraBrokerTestImageID     = "11111111-1111-4111-8111-111111111111"
)

func auroraBrokerTestTask() Task {
	return Task{
		ID:                auroraBrokerTestTaskID,
		GenerationID:      auroraBrokerTestGeneration,
		WorkspaceID:       auroraBrokerTestWorkspaceID,
		RuntimeID:         "rt-aurora",
		AgentID:           auroraBrokerTestAgentID,
		AuthToken:         "mat_aurora_broker_test",
		QuickCreatePrompt: "a poster about clouds",
		Agent: &AgentData{
			ID:        auroraBrokerTestAgentID,
			Name:      "aurora-text-image",
			SystemKey: "aurora:" + auroraBrokerTestSkill,
			// A hostile agent-level MCP config must never reach the broker launch.
			McpConfig: json.RawMessage("{\"mcpServers\":{\"evil\":{\"command\":\"sh\",\"args\":[\"-c\",\"echo pwned\"]}}}"),
		},
	}
}

// auroraBrokerTestTaskForSkill builds the same trusted task shape for another
// reviewed skill, optionally naming quick-create attachments.
func auroraBrokerTestTaskForSkill(skillID string, attachmentIDs ...string) Task {
	task := auroraBrokerTestTask()
	task.Agent.SystemKey = "aurora:" + skillID
	task.QuickCreateAttachmentIDs = append([]string(nil), attachmentIDs...)
	return task
}

// auroraBrokerTestFile is one attachment the fake server can serve. sizeOverride
// lets a case exercise the broker cap without allocating the whole object.
type auroraBrokerTestFile struct {
	filename     string
	contentType  string
	blob         []byte
	missing      bool
	sizeOverride int64
}

func newAuroraBrokerTestDaemon(t *testing.T) (*Daemon, string, string, func()) {
	return newAuroraBrokerTestDaemonWithAttachments(t, nil)
}

// newAuroraBrokerTestDaemonWithAttachments is newAuroraBrokerTestDaemon plus a
// fake task-token attachment surface: GET /api/attachments/{id} returns the
// metadata and /blob/{id} the bytes, matching the real relative download_url
// shape the daemon resolves against its client base URL.
func newAuroraBrokerTestDaemonWithAttachments(t *testing.T, files map[string]auroraBrokerTestFile) (*Daemon, string, string, func()) {
	t.Helper()

	testDir := t.TempDir()
	fakeBin := filepath.Join(testDir, "claude")
	argsFile := filepath.Join(testDir, "claude-args.txt")
	mcpFile := filepath.Join(testDir, "claude-mcp.json")
	script := "#!/bin/sh\n" +
		"{\n" +
		"  for arg in \"$@\"; do echo \"$arg\"; done\n" +
		"  echo --invocation-end--\n" +
		"} >> \"" + argsFile + "\"\n" +
		"mcp=\"\"\n" +
		"prev=\"\"\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--mcp-config\" ]; then mcp=\"$arg\"; fi\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"[ -n \"$mcp\" ] && cp \"$mcp\" \"" + mcpFile + "\"\n" +
		"IFS= read -r _\n" +
		"echo '{\"type\":\"system\",\"session_id\":\"s\"}'\n" +
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"s\",\"result\":\"done\"}'\n"
	writeTestExecutable(t, fakeBin, []byte(script))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/attachments/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/attachments/")
			file, ok := files[id]
			if !ok || file.missing {
				http.NotFound(w, r)
				return
			}
			size := int64(len(file.blob))
			if file.sizeOverride > 0 {
				size = file.sizeOverride
			}
			_ = json.NewEncoder(w).Encode(auroraAttachmentMetadata{
				ID:          id,
				Filename:    file.filename,
				ContentType: file.contentType,
				SizeBytes:   size,
				DownloadURL: "/blob/" + id,
			})
		case strings.HasPrefix(r.URL.Path, "/blob/"):
			id := strings.TrimPrefix(r.URL.Path, "/blob/")
			file, ok := files[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(file.blob)
		default:
			_, _ = w.Write([]byte("{}"))
		}
	}))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Daemon{
		client:         NewClient(srv.URL),
		logger:         logger,
		workspaces:     make(map[string]*workspaceState),
		runtimeIndex:   map[string]Runtime{"rt-aurora": {ID: "rt-aurora", Provider: "claude"}},
		activeEnvRoots: make(map[string]int),
		cfg: Config{
			WorkspacesRoot: t.TempDir(),
			AgentTimeout:   5 * time.Second,
			ServerBaseURL:  "https://api.aurora.example.test",
			Agents: map[string]AgentEntry{
				"claude": {Path: fakeBin},
			},
		},
	}
	return d, argsFile, mcpFile, srv.Close
}

// TestAuroraBrokerMcpConfigShape pins the one broker server the daemon injects:
// a fixed node entrypoint, a fixed argv, and exactly the compiled env allowlist.
func TestAuroraBrokerMcpConfigShape(t *testing.T) {
	t.Parallel()

	bc := auroraBrokerContext{
		SkillID:        auroraBrokerTestSkill,
		ServerOrigin:   "https://api.aurora.example.test",
		TaskID:         auroraBrokerTestTaskID,
		GenerationID:   auroraBrokerTestGeneration,
		WorkspaceID:    auroraBrokerTestWorkspaceID,
		Prompt:         "a poster about clouds",
		InputRoot:      auroraSandboxInputRoot,
		OutputRoot:     auroraSandboxOutputRoot,
		ContextPath:    "/data/workspaces/abc/workdir/.multica/aurora-broker/task-context.json",
		TaskTokenPath:  "/data/workspaces/abc/workdir/.multica/aurora-broker/task-token",
		ArkKeyFile:     auroraBrokerArkKeyFile,
		VolcASRKeyFile: auroraBrokerVolcASRKeyFile,
	}

	raw, err := auroraBrokerMcpConfig(bc, nil)
	if err != nil {
		t.Fatalf("auroraBrokerMcpConfig: %v", err)
	}

	var cfg struct {
		McpServers map[string]struct {
			Command string            "json:\"command\""
			Args    []string          "json:\"args\""
			Env     map[string]string "json:\"env\""
		} "json:\"mcpServers\""
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal broker config: %v", err)
	}
	if len(cfg.McpServers) != 1 {
		t.Fatalf("mcpServers = %v, want exactly one aurora server", cfg.McpServers)
	}
	server, ok := cfg.McpServers["aurora"]
	if !ok {
		t.Fatalf("mcpServers = %v, want key aurora", cfg.McpServers)
	}
	if server.Command != "node" {
		t.Errorf("command = %q, want node", server.Command)
	}
	if want := []string{auroraBrokerEntrypoint}; !slices.Equal(server.Args, want) {
		t.Errorf("args = %v, want %v", server.Args, want)
	}
	wantEnv := map[string]string{
		"AURORA_SERVER_ORIGIN":        bc.ServerOrigin,
		"AURORA_TASK_CONTEXT_FILE":    bc.ContextPath,
		"AURORA_INPUT_ROOT":           auroraSandboxInputRoot,
		"AURORA_OUTPUT_ROOT":          auroraSandboxOutputRoot,
		"AURORA_ARTIFACT_IMPORT_PATH": "/api/agent/tasks/" + bc.TaskID + "/aurora-artifacts/import",
		"ARK_API_KEY_FILE":            auroraBrokerArkKeyFile,
		"VOLC_ASR_API_KEY_FILE":       auroraBrokerVolcASRKeyFile,
		"AURORA_TASK_TOKEN_FILE":      bc.TaskTokenPath,
	}
	if !reflect.DeepEqual(server.Env, wantEnv) {
		t.Errorf("env = %v, want exactly %v", server.Env, wantEnv)
	}
}

// TestAuroraBrokerProxyEnv pins the one reviewed env addition: the Fleet node's
// egress-proxy variables plus NODE_USE_ENV_PROXY. Without them Node's fetch
// dials directly, the sandbox has no route out, and both the server-origin
// provider-run call and the ARK provider call fail with "fetch failed" (the
// live task-24 failure).
func TestAuroraBrokerProxyEnv(t *testing.T) {
	t.Parallel()

	if got := auroraBrokerProxyEnvFrom(func(string) (string, bool) { return "", false }); got != nil {
		t.Errorf("no proxy env = %v, want nil", got)
	}
	if got := auroraBrokerProxyEnvFrom(func(string) (string, bool) { return "   ", true }); got != nil {
		t.Errorf("blank proxy env = %v, want nil", got)
	}

	lookup := func(name string) (string, bool) {
		switch name {
		case "HTTP_PROXY":
			return "http://egress:3128", true
		case "NO_PROXY":
			return "egress,127.0.0.1,localhost", true
		}
		return "", false
	}
	want := map[string]string{
		"HTTP_PROXY":         "http://egress:3128",
		"NO_PROXY":           "egress,127.0.0.1,localhost",
		"NODE_USE_ENV_PROXY": "1",
	}
	if got := auroraBrokerProxyEnvFrom(lookup); !reflect.DeepEqual(got, want) {
		t.Errorf("proxy env = %v, want %v", got, want)
	}

	bc := auroraBrokerContext{
		SkillID:        auroraBrokerTestSkill,
		ServerOrigin:   "http://host.docker.internal:18102",
		TaskID:         auroraBrokerTestTaskID,
		GenerationID:   auroraBrokerTestGeneration,
		WorkspaceID:    auroraBrokerTestWorkspaceID,
		Prompt:         "a poster about clouds",
		InputRoot:      auroraSandboxInputRoot,
		OutputRoot:     auroraSandboxOutputRoot,
		ContextPath:    "/data/workspaces/abc/workdir/.multica/aurora-broker/task-context.json",
		TaskTokenPath:  "/data/workspaces/abc/workdir/.multica/aurora-broker/task-token",
		ArkKeyFile:     auroraBrokerArkKeyFile,
		VolcASRKeyFile: auroraBrokerVolcASRKeyFile,
	}
	raw, err := auroraBrokerMcpConfig(bc, want)
	if err != nil {
		t.Fatalf("auroraBrokerMcpConfig: %v", err)
	}
	var cfg struct {
		McpServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal broker config: %v", err)
	}
	env := cfg.McpServers[auroraBrokerServerName].Env
	for name, value := range want {
		if env[name] != value {
			t.Errorf("broker env %s = %q, want %q", name, env[name], value)
		}
	}
	if env["ARK_API_KEY_FILE"] != auroraBrokerArkKeyFile {
		t.Errorf("proxy env merge dropped the fixed env: %v", env)
	}
}

// TestAuroraBrokerMcpConfigFailsClosed proves a broker config is never built
// from a partially-known context: every field the broker launch needs is
// required, so the caller must fail the task instead of launching a wider
// surface.
func TestAuroraBrokerMcpConfigFailsClosed(t *testing.T) {
	t.Parallel()

	full := auroraBrokerContext{
		SkillID:        auroraBrokerTestSkill,
		ServerOrigin:   "https://api.aurora.example.test",
		TaskID:         auroraBrokerTestTaskID,
		GenerationID:   auroraBrokerTestGeneration,
		WorkspaceID:    auroraBrokerTestWorkspaceID,
		Prompt:         "a poster about clouds",
		InputRoot:      auroraSandboxInputRoot,
		OutputRoot:     auroraSandboxOutputRoot,
		ContextPath:    "/data/workspaces/abc/workdir/.multica/aurora-broker/task-context.json",
		TaskTokenPath:  "/data/workspaces/abc/workdir/.multica/aurora-broker/task-token",
		ArkKeyFile:     auroraBrokerArkKeyFile,
		VolcASRKeyFile: auroraBrokerVolcASRKeyFile,
	}

	cases := []struct {
		name   string
		mutate func(*auroraBrokerContext)
	}{
		{"missing server origin", func(bc *auroraBrokerContext) { bc.ServerOrigin = "" }},
		{"missing context path", func(bc *auroraBrokerContext) { bc.ContextPath = "" }},
		{"missing token path", func(bc *auroraBrokerContext) { bc.TaskTokenPath = "" }},
		{"missing task id", func(bc *auroraBrokerContext) { bc.TaskID = "" }},
		{"missing skill", func(bc *auroraBrokerContext) { bc.SkillID = "" }},
		{"missing input root", func(bc *auroraBrokerContext) { bc.InputRoot = "" }},
		{"missing output root", func(bc *auroraBrokerContext) { bc.OutputRoot = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bc := full
			tc.mutate(&bc)
			if _, err := auroraBrokerMcpConfig(bc, nil); err == nil {
				t.Fatalf("auroraBrokerMcpConfig(%s) succeeded, want fail-closed error", tc.name)
			}
		})
	}
}

// TestWriteAuroraBrokerContextWritesOwnerOnlyFiles pins the runner half: the
// context and the task token land under the task workdir, owner-only, with the
// fixed output root that attachAuroraArtifacts reads.
func TestWriteAuroraBrokerContextWritesOwnerOnlyFiles(t *testing.T) {
	t.Parallel()

	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), cfg: Config{ServerBaseURL: "https://api.aurora.example.test"}}
	workDir := t.TempDir()

	bc, err := d.writeAuroraBrokerContext(context.Background(), auroraBrokerTestTask(), execenv.Environment{WorkDir: workDir})
	if err != nil {
		t.Fatalf("writeAuroraBrokerContext: %v", err)
	}
	if bc.SkillID != auroraBrokerTestSkill {
		t.Errorf("SkillID = %q, want %q", bc.SkillID, auroraBrokerTestSkill)
	}
	if bc.OutputRoot != auroraSandboxOutputRoot {
		t.Errorf("OutputRoot = %q, want the manifest root %q", bc.OutputRoot, auroraSandboxOutputRoot)
	}

	info, err := os.Stat(bc.ContextPath)
	if err != nil {
		t.Fatalf("stat context: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o400 {
		t.Errorf("context mode = %04o, want 0400", got)
	}
	tokenInfo, err := os.Stat(bc.TaskTokenPath)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if got := tokenInfo.Mode().Perm(); got != 0o400 {
		t.Errorf("token mode = %04o, want 0400", got)
	}
	token, err := os.ReadFile(bc.TaskTokenPath)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if strings.TrimSpace(string(token)) != "mat_aurora_broker_test" {
		t.Errorf("token = %q, want the task auth token", strings.TrimSpace(string(token)))
	}

	raw, err := os.ReadFile(bc.ContextPath)
	if err != nil {
		t.Fatalf("read context: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse context: %v", err)
	}
	for key, want := range map[string]any{
		"schema":          "com.multica.aurora.task-context",
		"version":         float64(1),
		"task_id":         auroraBrokerTestTaskID,
		"generation_id":   auroraBrokerTestGeneration,
		"workspace_id":    auroraBrokerTestWorkspaceID,
		"skill_id":        auroraBrokerTestSkill,
		"prompt":          "a poster about clouds",
		"output_root":     auroraSandboxOutputRoot,
		"server_origin":   "https://api.aurora.example.test",
		"task_token_file": bc.TaskTokenPath,
	} {
		if got := parsed[key]; got != want {
			t.Errorf("context[%q] = %v, want %v", key, got, want)
		}
	}
	if _, ok := parsed["attachments"].(map[string]any); !ok {
		t.Errorf("context attachments = %v, want an object", parsed["attachments"])
	}
	// The broker rejects unknown fields, so the daemon must write exactly the
	// compiled schema.
	wantKeys := []string{"schema", "version", "task_id", "generation_id", "workspace_id", "skill_id", "prompt", "attachments", "output_root", "server_origin", "task_token_file"}
	if len(parsed) != len(wantKeys) {
		t.Errorf("context keys = %v, want %v", parsed, wantKeys)
	}
	for _, key := range wantKeys {
		if _, ok := parsed[key]; !ok {
			t.Errorf("context missing %q", key)
		}
	}
}

// TestWriteAuroraBrokerContextRequiresGenerationID pins the contract field: the
// real Aurora generation id must be forwarded, and the task id is never a
// stand-in, because the completion and artifact routes key on the generation.
func TestWriteAuroraBrokerContextRequiresGenerationID(t *testing.T) {
	t.Parallel()

	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), cfg: Config{ServerBaseURL: "https://api.aurora.example.test"}}
	task := auroraBrokerTestTask()
	task.GenerationID = ""
	if _, err := d.writeAuroraBrokerContext(context.Background(), task, execenv.Environment{WorkDir: t.TempDir()}); err == nil {
		t.Fatal("writeAuroraBrokerContext with an empty generation id succeeded, want fail-closed error")
	}

	task.GenerationID = auroraBrokerTestGeneration
	bc, err := d.writeAuroraBrokerContext(context.Background(), task, execenv.Environment{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("writeAuroraBrokerContext: %v", err)
	}
	if bc.GenerationID != auroraBrokerTestGeneration {
		t.Fatalf("GenerationID = %q, want %q", bc.GenerationID, auroraBrokerTestGeneration)
	}
}

// TestWriteAuroraBrokerContextFailsClosed proves a missing identity or token
// stops the task before any agent starts, rather than writing a context the
// broker would reject at launch (or worse, an empty allowlist).
func TestWriteAuroraBrokerContextFailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*Daemon, *Task, *execenv.Environment)
	}{
		{"missing workdir", func(_ *Daemon, _ *Task, env *execenv.Environment) { env.WorkDir = "" }},
		{"missing auth token", func(_ *Daemon, task *Task, _ *execenv.Environment) { task.AuthToken = "" }},
		{"missing skill key", func(_ *Daemon, task *Task, _ *execenv.Environment) { task.Agent.SystemKey = "" }},
		{"unknown skill", func(_ *Daemon, task *Task, _ *execenv.Environment) { task.Agent.SystemKey = "aurora:not-a-skill" }},
		{"missing server origin", func(d *Daemon, _ *Task, _ *execenv.Environment) { d.cfg.ServerBaseURL = "" }},
		{"missing generation id", func(_ *Daemon, task *Task, _ *execenv.Environment) { task.GenerationID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), cfg: Config{ServerBaseURL: "https://api.aurora.example.test"}}
			task := auroraBrokerTestTask()
			env := execenv.Environment{WorkDir: t.TempDir()}
			tc.mutate(d, &task, &env)
			if _, err := d.writeAuroraBrokerContext(context.Background(), task, env); err == nil {
				t.Fatalf("writeAuroraBrokerContext(%s) succeeded, want fail-closed error", tc.name)
			}
		})
	}
}

// TestAuroraBrokerRunTaskInjectsSurface drives a real runTask against a fake
// claude CLI and pins the launch: the reviewed allowed set reaches
// --allowedTools, the broker MCP config (and only the broker) is passed through
// --mcp-config with strict mode, and a hostile agent-level mcp_config is
// discarded.
func TestAuroraBrokerRunTaskInjectsSurface(t *testing.T) {
	t.Parallel()

	d, argsFile, mcpFile, cleanup := newAuroraBrokerTestDaemon(t)
	defer cleanup()

	task := auroraBrokerTestTask()
	result, err := d.runTask(context.Background(), task, "claude", 0, d.logger)
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("result status = %q, want completed: %+v", result.Status, result)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read claude args: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if !slices.Contains(lines, "--allowedTools") {
		t.Fatalf("argv missing --allowedTools:\n%s", args)
	}
	atIdx := slices.Index(lines, "--allowedTools")
	if atIdx+1 >= len(lines) {
		t.Fatalf("--allowedTools has no value:\n%s", args)
	}
	// Claude Code addresses MCP tools as mcp__<server>__<tool>; a bare broker
	// method name approves nothing, so the exact identifier is load-bearing.
	if want := auroraBrokerMCPToolName("aurora.seedream_generate"); lines[atIdx+1] != want {
		t.Fatalf("--allowedTools = %q, want exactly %q", lines[atIdx+1], want)
	}
	if !slices.Contains(lines, "--disallowedTools") {
		t.Fatalf("argv missing --disallowedTools:\n%s", args)
	}
	dtIdx := slices.Index(lines, "--disallowedTools")
	if dtIdx+1 >= len(lines) || !strings.Contains(lines[dtIdx+1], "Bash") {
		t.Fatalf("--disallowedTools = %v, want Bash denied", lines)
	}
	permIdx := slices.Index(lines, "--permission-mode")
	if permIdx+1 >= len(lines) || lines[permIdx+1] != "default" {
		t.Fatalf("--permission-mode = %v, want default", lines)
	}
	if !slices.Contains(lines, "--strict-mcp-config") {
		t.Fatalf("argv missing --strict-mcp-config:\n%s", args)
	}

	mcpRaw, err := os.ReadFile(mcpFile)
	if err != nil {
		t.Fatalf("read captured mcp config: %v", err)
	}
	var cfg struct {
		McpServers map[string]json.RawMessage "json:\"mcpServers\""
	}
	if err := json.Unmarshal(mcpRaw, &cfg); err != nil {
		t.Fatalf("parse captured mcp config: %v", err)
	}
	if len(cfg.McpServers) != 1 {
		t.Fatalf("mcpServers = %v, want only the broker", cfg.McpServers)
	}
	if _, ok := cfg.McpServers["aurora"]; !ok {
		t.Fatalf("mcpServers = %v, want the aurora broker", cfg.McpServers)
	}
	if _, ok := cfg.McpServers["evil"]; ok {
		t.Fatalf("hostile agent mcp_config survived on an Aurora task: %s", mcpRaw)
	}

	contextRaw, err := os.ReadFile(filepath.Join(result.WorkDir, filepath.FromSlash(auroraBrokerStateRelDir), auroraBrokerContextFileName))
	if err != nil {
		t.Fatalf("read written task context: %v", err)
	}
	if !strings.Contains(string(contextRaw), "\"output_root\":\""+auroraSandboxOutputRoot+"\"") {
		t.Fatalf("task context does not name the manifest output root: %s", contextRaw)
	}
}

// TestAuroraBrokerNonAuroraMcpConfigUnchanged pins the additive boundary: an
// ordinary user agent keeps its merged MCP configuration and never receives the
// Aurora allowlist.
func TestAuroraBrokerNonAuroraMcpConfigUnchanged(t *testing.T) {
	t.Parallel()

	d, argsFile, mcpFile, cleanup := newAuroraBrokerTestDaemon(t)
	defer cleanup()

	task := Task{
		ID:          "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		WorkspaceID: auroraBrokerTestWorkspaceID,
		RuntimeID:   "rt-aurora",
		AgentID:     "ffffffff-ffff-4fff-8fff-ffffffffffff",
		AuthToken:   "mat_plain_agent",
		Agent: &AgentData{
			ID:        "ffffffff-ffff-4fff-8fff-ffffffffffff",
			Name:      "plain-agent",
			McpConfig: json.RawMessage("{\"mcpServers\":{\"keep\":{\"command\":\"keep-server\"}}}"),
		},
	}
	if _, err := d.runTask(context.Background(), task, "claude", 0, d.logger); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read claude args: %v", err)
	}
	if strings.Contains(string(args), "--allowedTools") {
		t.Fatalf("non-Aurora task received the Aurora allowlist:\n%s", args)
	}
	mcpRaw, err := os.ReadFile(mcpFile)
	if err != nil {
		t.Fatalf("read captured mcp config: %v", err)
	}
	if !strings.Contains(string(mcpRaw), "\"keep\"") {
		t.Fatalf("non-Aurora task lost its agent mcp_config: %s", mcpRaw)
	}
	if strings.Contains(string(mcpRaw), "aurora") {
		t.Fatalf("non-Aurora task received the Aurora broker: %s", mcpRaw)
	}
}

// TestAuroraBrokerRunTaskStagesAttachments is the fake-CLI regression for a
// skill whose policy requires one input: the daemon stages the attachment into
// the broker input root, the written context carries the exact broker
// attachments entry, and --allowedTools names the skill's MCP identifier.
func TestAuroraBrokerRunTaskStagesAttachments(t *testing.T) {
	t.Parallel()

	blob := []byte("reference-bytes")
	d, argsFile, _, cleanup := newAuroraBrokerTestDaemonWithAttachments(t, map[string]auroraBrokerTestFile{
		auroraBrokerTestImageID: {filename: "reference.png", contentType: "image/png", blob: blob},
	})
	defer cleanup()
	inputRoot := t.TempDir()
	d.auroraInputRoot = inputRoot

	task := auroraBrokerTestTaskForSkill("id-photo", auroraBrokerTestImageID)
	result, err := d.runTask(context.Background(), task, "claude", 0, d.logger)
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("result status = %q, want completed: %+v", result.Status, result)
	}

	contextRaw, err := os.ReadFile(filepath.Join(result.WorkDir, filepath.FromSlash(auroraBrokerStateRelDir), auroraBrokerContextFileName))
	if err != nil {
		t.Fatalf("read written task context: %v", err)
	}
	var parsed struct {
		GenerationID string `json:"generation_id"`
		Attachments  map[string]struct {
			RelativePath string `json:"relative_path"`
			MIMEType     string `json:"mime_type"`
			SizeBytes    int64  `json:"size_bytes"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(contextRaw, &parsed); err != nil {
		t.Fatalf("parse task context: %v", err)
	}
	if parsed.GenerationID != auroraBrokerTestGeneration {
		t.Errorf("generation_id = %q, want %q", parsed.GenerationID, auroraBrokerTestGeneration)
	}
	entry, ok := parsed.Attachments[auroraBrokerTestImageID]
	if !ok {
		t.Fatalf("attachments = %v, want key %s", parsed.Attachments, auroraBrokerTestImageID)
	}
	wantRelative := auroraBrokerTestImageID + ".png"
	if entry.RelativePath != wantRelative || entry.MIMEType != "image/png" || entry.SizeBytes != int64(len(blob)) {
		t.Fatalf("attachment = %+v, want relative_path=%s mime_type=image/png size_bytes=%d", entry, wantRelative, len(blob))
	}
	staged, err := os.ReadFile(filepath.Join(inputRoot, wantRelative))
	if err != nil {
		t.Fatalf("read staged attachment: %v", err)
	}
	if !reflect.DeepEqual(staged, blob) {
		t.Fatalf("staged bytes = %q, want %q", staged, blob)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read claude args: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	atIdx := slices.Index(lines, "--allowedTools")
	if atIdx < 0 || atIdx+1 >= len(lines) || lines[atIdx+1] != auroraBrokerMCPToolName("aurora.id_photo") {
		t.Fatalf("--allowedTools = %v, want exactly %q", lines, auroraBrokerMCPToolName("aurora.id_photo"))
	}
}

// TestStageAuroraBrokerAttachmentsFailsClosed proves the broker is never handed
// a context its loader would reject: an unresolvable id, an unsupported
// extension, an empty object, a policy violation, or an over-cap file all fail
// the task before launch.
func TestStageAuroraBrokerAttachmentsFailsClosed(t *testing.T) {
	t.Parallel()

	secondImageID := "22222222-2222-4222-8222-222222222222"
	blob := []byte("bytes")

	cases := []struct {
		name  string
		skill string
		ids   []string
		files map[string]auroraBrokerTestFile
	}{
		{
			name:  "missing required attachment",
			skill: "id-photo",
		},
		{
			name:  "unresolvable attachment",
			skill: "id-photo",
			ids:   []string{auroraBrokerTestImageID},
			files: map[string]auroraBrokerTestFile{auroraBrokerTestImageID: {filename: "reference.png", contentType: "image/png", missing: true}},
		},
		{
			name:  "unsupported extension",
			skill: "id-photo",
			ids:   []string{auroraBrokerTestImageID},
			files: map[string]auroraBrokerTestFile{auroraBrokerTestImageID: {filename: "reference.bmp", contentType: "image/bmp", blob: blob}},
		},
		{
			name:  "empty object",
			skill: "id-photo",
			ids:   []string{auroraBrokerTestImageID},
			files: map[string]auroraBrokerTestFile{auroraBrokerTestImageID: {filename: "reference.png", contentType: "image/png", blob: nil}},
		},
		{
			name:  "kind not accepted by skill",
			skill: "id-photo",
			ids:   []string{auroraBrokerTestImageID},
			files: map[string]auroraBrokerTestFile{auroraBrokerTestImageID: {filename: "clip.mp4", contentType: "video/mp4", blob: blob}},
		},
		{
			name:  "more than the policy maximum",
			skill: "id-photo",
			ids:   []string{auroraBrokerTestImageID, secondImageID},
			files: map[string]auroraBrokerTestFile{
				auroraBrokerTestImageID: {filename: "one.png", contentType: "image/png", blob: blob},
				secondImageID:           {filename: "two.png", contentType: "image/png", blob: blob},
			},
		},
		{
			name:  "duplicate ids",
			skill: "id-photo",
			ids:   []string{auroraBrokerTestImageID, auroraBrokerTestImageID},
			files: map[string]auroraBrokerTestFile{auroraBrokerTestImageID: {filename: "one.png", contentType: "image/png", blob: blob}},
		},
		{
			name:  "over the broker size cap",
			skill: "id-photo",
			ids:   []string{auroraBrokerTestImageID},
			files: map[string]auroraBrokerTestFile{auroraBrokerTestImageID: {filename: "huge.png", contentType: "image/png", blob: blob, sizeOverride: (25 << 20) + 1}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, cleanup := newAuroraBrokerTestDaemonWithAttachments(t, tc.files)
			defer cleanup()
			d.auroraInputRoot = t.TempDir()
			task := auroraBrokerTestTaskForSkill(tc.skill, tc.ids...)
			if _, err := d.writeAuroraBrokerContext(context.Background(), task, execenv.Environment{WorkDir: t.TempDir()}); err == nil {
				t.Fatalf("writeAuroraBrokerContext(%s) succeeded, want a fail-closed staging error", tc.name)
			}
		})
	}
}

// TestStageAuroraBrokerAttachmentsPopulatesBrokerShape is the unit half of the
// min>=1 regression: a valid image set is staged and marshalled into exactly the
// relative_path/mime_type/size_bytes shape loadTaskContext reads.
func TestStageAuroraBrokerAttachmentsPopulatesBrokerShape(t *testing.T) {
	t.Parallel()

	blob := []byte("reference-bytes")
	d, _, _, cleanup := newAuroraBrokerTestDaemonWithAttachments(t, map[string]auroraBrokerTestFile{
		auroraBrokerTestImageID: {filename: "reference.PNG", contentType: "image/png", blob: blob},
	})
	defer cleanup()
	inputRoot := t.TempDir()
	d.auroraInputRoot = inputRoot

	bc, err := d.writeAuroraBrokerContext(context.Background(), auroraBrokerTestTaskForSkill("id-photo", auroraBrokerTestImageID), execenv.Environment{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("writeAuroraBrokerContext: %v", err)
	}
	if bc.InputRoot != inputRoot {
		t.Fatalf("InputRoot = %q, want %q", bc.InputRoot, inputRoot)
	}
	raw, err := os.ReadFile(bc.ContextPath)
	if err != nil {
		t.Fatalf("read context: %v", err)
	}
	var parsed struct {
		Attachments map[string]struct {
			RelativePath string `json:"relative_path"`
			MIMEType     string `json:"mime_type"`
			SizeBytes    int64  `json:"size_bytes"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse context: %v", err)
	}
	entry := parsed.Attachments[auroraBrokerTestImageID]
	if entry.RelativePath != auroraBrokerTestImageID+".png" || entry.MIMEType != "image/png" || entry.SizeBytes != int64(len(blob)) {
		t.Fatalf("attachment = %+v, want relative_path=%s.png mime_type=image/png size_bytes=%d", entry, auroraBrokerTestImageID, len(blob))
	}
	if _, err := os.Stat(filepath.Join(inputRoot, entry.RelativePath)); err != nil {
		t.Fatalf("staged attachment missing: %v", err)
	}
}
