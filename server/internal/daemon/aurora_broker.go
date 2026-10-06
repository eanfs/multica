package daemon

// Aurora MCP broker injection (Task 6).
//
// The 9 aurora.* tools live in the sandbox's stdio MCP server
// (deploy/aurora-sandbox/runtime/src/server.mjs). This file makes that server
// the only execution surface an Aurora task's Claude child can reach: it writes
// the broker's bounded task context and task token under the task workdir and
// builds the fixed mcpServers config the daemon passes via --mcp-config. The
// reviewed allowlist travels separately as ExecOptions.AllowedTools, so the
// general-purpose tools stay denied and the broker tools are the only ones
// Claude may call.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/util"
)

// auroraBrokerEntrypoint is the fixed Node entry point the managed sandbox
// image installs. deploy/aurora-sandbox/Dockerfile copies the pnpm workspace
// root to /opt/aurora/runtime, so the runtime tree keeps its repository path.
const auroraBrokerEntrypoint = "/opt/aurora/runtime/deploy/aurora-sandbox/runtime/src/server.mjs"

// Fixed mounts and compiled secret paths the broker reads. They are compiled
// constants, never derived from the task, a prompt, or an agent payload, so a
// hostile record cannot retarget the broker at another origin, root, or file.
const (
	// auroraSandboxInputRoot matches the broker default (DEFAULT_INPUT_ROOT).
	auroraSandboxInputRoot = "/workspace/input"

	// The provider key files are the read-only binds the Fleet node mounts at
	// the fixed /run/secrets paths (see the Runtime source defaults).
	auroraBrokerArkKeyFile     = "/run/secrets/ark-api-key"
	auroraBrokerOpenAIKeyFile  = "/run/secrets/openai-api-key"
	auroraBrokerVolcASRKeyFile = "/run/secrets/volc-asr-api-key"

	// The context and token files the daemon writes under the task workdir. The
	// broker reads both by absolute path through its env; neither is shared
	// between tasks.
	auroraBrokerStateRelDir     = ".multica/aurora-broker"
	auroraBrokerContextFileName = "task-context.json"
	auroraBrokerTokenFileName   = "task-token"

	auroraBrokerContextSchema  = "com.multica.aurora.task-context"
	auroraBrokerContextVersion = 1
)

// errAuroraBrokerContextInvalid is returned when an Aurora task cannot produce
// the broker's bounded context. runTask returns it before launch, so the task
// fails (and refunds) instead of running with an empty or wider surface.
var errAuroraBrokerContextInvalid = errors.New("aurora: broker context is incomplete")

// auroraBrokerContext is the resolved runtime context the broker needs. It is
// written to the mode-0400 context JSON; the paths live in the broker env.
type auroraBrokerContext struct {
	SkillID        string
	ServerOrigin   string
	TaskID         string
	GenerationID   string
	WorkspaceID    string
	Prompt         string
	InputRoot      string
	OutputRoot     string
	ContextPath    string
	TaskTokenPath  string
	ArkKeyFile     string
	OpenAIKeyFile  string
	VolcASRKeyFile string
}

// auroraBrokerServerConfig is one stdio MCP server entry in the Claude
// --mcp-config file.
type auroraBrokerServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

type auroraBrokerMcpConfigFile struct {
	McpServers map[string]auroraBrokerServerConfig `json:"mcpServers"`
}

// auroraBrokerImportPath is the task-token artifact importer the broker calls
// for provider result URLs. It is fixed by the route shape and the task id, so
// the model can never supply or widen it.
func auroraBrokerImportPath(taskID string) string {
	return "/api/agent/tasks/" + taskID + "/aurora-artifacts/import"
}

// auroraBrokerMcpConfig builds the fixed Claude MCP config for the reviewed
// broker. It fails closed on any missing input: a partially-resolved context
// must fail the task, never launch with an unknown command, root, or secret.
func auroraBrokerMcpConfig(bc auroraBrokerContext) (json.RawMessage, error) {
	required := []struct {
		name  string
		value string
	}{
		{"server origin", bc.ServerOrigin},
		{"task id", bc.TaskID},
		{"skill id", bc.SkillID},
		{"context path", bc.ContextPath},
		{"task token path", bc.TaskTokenPath},
		{"input root", bc.InputRoot},
		{"output root", bc.OutputRoot},
		{"ark key file", bc.ArkKeyFile},
		{"openai key file", bc.OpenAIKeyFile},
		{"volc asr key file", bc.VolcASRKeyFile},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return nil, fmt.Errorf("%w: missing %s", errAuroraBrokerContextInvalid, field.name)
		}
	}

	cfg := auroraBrokerMcpConfigFile{McpServers: map[string]auroraBrokerServerConfig{
		"aurora": {
			Command: "node",
			Args:    []string{auroraBrokerEntrypoint},
			// Exactly the compiled broker env allowlist. Adding any other key
			// (HOME, PATH, provider credentials) would widen the child's view
			// beyond the reviewed surface.
			Env: map[string]string{
				"AURORA_SERVER_ORIGIN":        bc.ServerOrigin,
				"AURORA_TASK_CONTEXT_FILE":    bc.ContextPath,
				"AURORA_INPUT_ROOT":           bc.InputRoot,
				"AURORA_OUTPUT_ROOT":          bc.OutputRoot,
				"AURORA_ARTIFACT_IMPORT_PATH": auroraBrokerImportPath(bc.TaskID),
				"ARK_API_KEY_FILE":            bc.ArkKeyFile,
				"OPENAI_API_KEY_FILE":         bc.OpenAIKeyFile,
				"VOLC_ASR_API_KEY_FILE":       bc.VolcASRKeyFile,
				"AURORA_TASK_TOKEN_FILE":      bc.TaskTokenPath,
			},
		},
	}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal broker MCP config: %v", errAuroraBrokerContextInvalid, err)
	}
	return raw, nil
}

// writeAuroraBrokerContext writes the broker's mode-0400 context JSON and the
// mode-0400 task token under env.WorkDir, then returns the resolved context the
// MCP config is built from. It is fail-closed: every missing or non-UUID
// identity, absent token, or unknown skill is an error the caller turns into a
// task failure (the existing refund path).
//
// generation_id: the claim endpoint does not yet forward the Aurora generation
// id, and the daemon cannot reach the database. The server resolves the real
// generation from the task id on every task-token call, so the task id is the
// only stable UUID available here; it is written as an opaque identity the
// broker validates, never used to authorize anything.
func (d *Daemon) writeAuroraBrokerContext(task Task, env execenv.Environment) (auroraBrokerContext, error) {
	skillID, ok := auroraSkillID(task)
	if !ok {
		return auroraBrokerContext{}, fmt.Errorf("%w: no trusted skill id", errAuroraBrokerContextInvalid)
	}
	if _, ok := aurora.ExecutionPolicy(skillID); !ok {
		return auroraBrokerContext{}, fmt.Errorf("%w: skill %q is not executable", errAuroraBrokerContextInvalid, skillID)
	}
	workDir := strings.TrimSpace(env.WorkDir)
	if workDir == "" {
		return auroraBrokerContext{}, fmt.Errorf("%w: task workdir is required", errAuroraBrokerContextInvalid)
	}
	serverOrigin := strings.TrimRight(strings.TrimSpace(d.cfg.ServerBaseURL), "/")
	if serverOrigin == "" {
		return auroraBrokerContext{}, fmt.Errorf("%w: server origin is required", errAuroraBrokerContextInvalid)
	}
	token := strings.TrimSpace(task.AuthToken)
	if token == "" {
		return auroraBrokerContext{}, fmt.Errorf("%w: task auth token is required", errAuroraBrokerContextInvalid)
	}
	taskID, err := auroraBrokerUUID(task.ID, "task id")
	if err != nil {
		return auroraBrokerContext{}, err
	}
	workspaceID, err := auroraBrokerUUID(task.WorkspaceID, "workspace id")
	if err != nil {
		return auroraBrokerContext{}, err
	}
	generationID := strings.TrimSpace(task.GenerationID)
	if generationID == "" {
		generationID = taskID
	}
	generationID, err = auroraBrokerUUID(generationID, "generation id")
	if err != nil {
		return auroraBrokerContext{}, err
	}
	prompt := task.QuickCreatePrompt
	if strings.TrimSpace(prompt) == "" {
		return auroraBrokerContext{}, fmt.Errorf("%w: task prompt is required", errAuroraBrokerContextInvalid)
	}

	stateDir := filepath.Join(workDir, filepath.FromSlash(auroraBrokerStateRelDir))
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return auroraBrokerContext{}, fmt.Errorf("%w: create broker state dir: %v", errAuroraBrokerContextInvalid, err)
	}
	contextPath := filepath.Join(stateDir, auroraBrokerContextFileName)
	tokenPath := filepath.Join(stateDir, auroraBrokerTokenFileName)

	// 0400 files cannot be overwritten by their owner, so clear a leftover from
	// a reused workdir before recreating it with the exact mode.
	for _, path := range []string{tokenPath, contextPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return auroraBrokerContext{}, fmt.Errorf("%w: clear %s: %v", errAuroraBrokerContextInvalid, filepath.Base(path), err)
		}
	}

	if err := os.WriteFile(tokenPath, []byte(token), 0o400); err != nil {
		return auroraBrokerContext{}, fmt.Errorf("%w: write task token: %v", errAuroraBrokerContextInvalid, err)
	}
	if err := os.Chmod(tokenPath, 0o400); err != nil {
		return auroraBrokerContext{}, fmt.Errorf("%w: restrict task token: %v", errAuroraBrokerContextInvalid, err)
	}

	// The broker's loadTaskContext rejects unknown fields and a non-object
	// attachments map, so this is the exact compiled schema.
	context := map[string]any{
		"schema":          auroraBrokerContextSchema,
		"version":         auroraBrokerContextVersion,
		"task_id":         taskID,
		"generation_id":   generationID,
		"workspace_id":    workspaceID,
		"skill_id":        skillID,
		"prompt":          prompt,
		"attachments":     map[string]any{},
		"output_root":     auroraSandboxOutputRoot,
		"server_origin":   serverOrigin,
		"task_token_file": tokenPath,
	}
	raw, err := json.Marshal(context)
	if err != nil {
		return auroraBrokerContext{}, fmt.Errorf("%w: marshal task context: %v", errAuroraBrokerContextInvalid, err)
	}
	if err := os.WriteFile(contextPath, raw, 0o400); err != nil {
		return auroraBrokerContext{}, fmt.Errorf("%w: write task context: %v", errAuroraBrokerContextInvalid, err)
	}
	if err := os.Chmod(contextPath, 0o400); err != nil {
		return auroraBrokerContext{}, fmt.Errorf("%w: restrict task context: %v", errAuroraBrokerContextInvalid, err)
	}

	return auroraBrokerContext{
		SkillID:        skillID,
		ServerOrigin:   serverOrigin,
		TaskID:         taskID,
		GenerationID:   generationID,
		WorkspaceID:    workspaceID,
		Prompt:         prompt,
		InputRoot:      auroraSandboxInputRoot,
		OutputRoot:     auroraSandboxOutputRoot,
		ContextPath:    contextPath,
		TaskTokenPath:  tokenPath,
		ArkKeyFile:     auroraBrokerArkKeyFile,
		OpenAIKeyFile:  auroraBrokerOpenAIKeyFile,
		VolcASRKeyFile: auroraBrokerVolcASRKeyFile,
	}, nil
}

// auroraBrokerUUID validates and canonicalizes a task identity. The broker's
// context loader requires lowercase-form UUIDs, so a malformed value fails the
// task here instead of at broker startup.
func auroraBrokerUUID(value, label string) (string, error) {
	parsed, err := util.ParseUUID(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("%w: %s is not a UUID", errAuroraBrokerContextInvalid, label)
	}
	return util.UUIDToString(parsed), nil
}
