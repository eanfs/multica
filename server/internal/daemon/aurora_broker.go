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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

	// auroraBrokerServerName is the mcpServers key the daemon injects. Claude
	// Code qualifies every MCP tool as mcp__<server>__<tool> with each segment
	// sanitized by auroraBrokerMCPNameSegment, so the reviewed allowlist in
	// ExecOptions.AllowedTools must use that sanitized identifier.
	auroraBrokerServerName = "aurora"
)

// auroraBrokerAttachmentType is the broker's exact extension -> (MIME, kind)
// contract from deploy/aurora-sandbox/runtime/src/policy.mjs (KIND_TYPES). A
// staged file only passes the broker's classifyAttachment when its extension
// and the declared mime_type are one of these pairs, so the daemon derives the
// relative path extension from this table and never trusts the uploader's
// content type for the pair.
type auroraBrokerAttachmentType struct {
	mimeType string
	kind     aurora.AttachmentKind
}

var auroraBrokerAttachmentTypes = map[string]auroraBrokerAttachmentType{
	".png":      {"image/png", aurora.AttachmentImage},
	".jpg":      {"image/jpeg", aurora.AttachmentImage},
	".jpeg":     {"image/jpeg", aurora.AttachmentImage},
	".txt":      {"text/plain", aurora.AttachmentDocument},
	".md":       {"text/markdown", aurora.AttachmentDocument},
	".markdown": {"text/markdown", aurora.AttachmentDocument},
	".pdf":      {"application/pdf", aurora.AttachmentDocument},
	".docx":     {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", aurora.AttachmentDocument},
	".wav":      {"audio/wav", aurora.AttachmentAudio},
	".mp3":      {"audio/mpeg", aurora.AttachmentAudio},
	".ogg":      {"audio/ogg", aurora.AttachmentAudio},
	".opus":     {"audio/opus", aurora.AttachmentAudio},
	".mp4":      {"video/mp4", aurora.AttachmentVideo},
	".mov":      {"video/quicktime", aurora.AttachmentVideo},
	".webm":     {"video/webm", aurora.AttachmentVideo},
}

// auroraBrokerAttachmentMaxBytes mirrors policy.mjs kindLimit: the per-kind cap
// the broker enforces on the declared size_bytes.
func auroraBrokerAttachmentMaxBytes(kind aurora.AttachmentKind) (int64, bool) {
	switch kind {
	case aurora.AttachmentImage:
		return 25 << 20, true
	case aurora.AttachmentDocument:
		return 25 << 20, true
	case aurora.AttachmentAudio:
		return 100 << 20, true
	case aurora.AttachmentVideo:
		return 100 << 20, true
	default:
		return 0, false
	}
}

// auroraBrokerAttachment is one entry of the broker context's attachments map.
// Its JSON shape is exactly the broker's ATTACHMENT_FIELDS contract; adding a
// field would make loadTaskContext reject the whole context.
type auroraBrokerAttachment struct {
	RelativePath string `json:"relative_path"`
	MIMEType     string `json:"mime_type"`
	SizeBytes    int64  `json:"size_bytes"`
}

// auroraBrokerMCPNameSegment reproduces Claude Code's MCP name sanitization
// exactly as the pinned CLI (2.1.282, the version the sandbox image installs)
// implements it: every character outside [A-Za-z0-9_-] is replaced with "_".
// The CLI qualifies an MCP tool as mcp__<sanitized(server)>__<sanitized(tool)>
// and matches --allowedTools against that sanitized identifier. The broker's
// reviewed methods are namespaced "aurora.<verb>", so the dot in each method
// becomes "_" in the identifier the model is told to call and in the allowlist;
// the CLI still forwards the broker's original "aurora.<verb>" name downstream
// (proven against a scratch server), so server.mjs keeps registering the dotted
// names and policy.mjs keeps keying on them.
func auroraBrokerMCPNameSegment(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// auroraBrokerMCPToolName maps a broker tool method to the identifier Claude
// Code accepts in --allowedTools. The CLI's tool-name validator is
// ^mcp__[\w-]+(?:__(?:[\w.-]+|\*))?$ (Claude Code 2.1.282), but validation
// is not qualification: the name actually registered and matched is the
// sanitized mcp__<server>__<tool> form, so every method segment must pass
// through auroraBrokerMCPNameSegment before it reaches a prompt or a
// permission list.
func auroraBrokerMCPToolName(method string) string {
	return "mcp__" + auroraBrokerMCPNameSegment(auroraBrokerServerName) + "__" + auroraBrokerMCPNameSegment(method)
}

// auroraBrokerInputRootPath is the root the daemon stages inputs in and names
// in AURORA_INPUT_ROOT. Production uses the fixed broker default; a test may
// point it at a temp dir so the default suite never writes /workspace.
func (d *Daemon) auroraBrokerInputRootPath() string {
	if root := strings.TrimSpace(d.auroraInputRoot); root != "" {
		return root
	}
	return auroraSandboxInputRoot
}

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

// auroraBrokerProxyEnvVars are the egress-proxy variables the Fleet node gives
// the sandbox so every process reaches the server and the provider origins. The
// broker must get them too: the sandbox has no direct route out, so Node's
// fetch without the proxy fails with EAI_AGAIN and the broker never reaches the
// server-origin importer/provider-run endpoints or the ARK provider. They are
// the node's own reviewed egress wiring, not a new capability, and they carry
// no credential.
var auroraBrokerProxyEnvVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}

// auroraBrokerProxyEnvFrom returns the broker's proxy environment. It is empty
// when the node has no proxy configured (the broker then uses direct fetch),
// and otherwise adds NODE_USE_ENV_PROXY=1: Node 22's global fetch ignores the
// standard proxy variables until that flag is set, and the flag is what makes
// the provider HTTPS path CONNECT through the egress sidecar. The server-origin
// HTTP path is handled by the broker's own absolute-form fetch.
func auroraBrokerProxyEnvFrom(lookup func(string) (string, bool)) map[string]string {
	env := map[string]string{}
	for _, name := range auroraBrokerProxyEnvVars {
		if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
			env[name] = value
		}
	}
	if len(env) == 0 {
		return nil
	}
	env["NODE_USE_ENV_PROXY"] = "1"
	return env
}

// auroraBrokerProxyEnv reads the proxy environment from the daemon's own
// process environment, which is the Fleet node's container environment.
func auroraBrokerProxyEnv() map[string]string {
	return auroraBrokerProxyEnvFrom(os.LookupEnv)
}

// auroraBrokerEnv is the broker's exact environment: the compiled broker
// contract plus the node's egress-proxy variables. It never copies the daemon
// environment wholesale, so HOME, PATH, provider credentials, and every other
// host variable stay outside the child's view.
func auroraBrokerEnv(bc auroraBrokerContext, proxyEnv map[string]string) map[string]string {
	env := map[string]string{
		"AURORA_SERVER_ORIGIN":        bc.ServerOrigin,
		"AURORA_TASK_CONTEXT_FILE":    bc.ContextPath,
		"AURORA_INPUT_ROOT":           bc.InputRoot,
		"AURORA_OUTPUT_ROOT":          bc.OutputRoot,
		"AURORA_ARTIFACT_IMPORT_PATH": auroraBrokerImportPath(bc.TaskID),
		"ARK_API_KEY_FILE":            bc.ArkKeyFile,
		"OPENAI_API_KEY_FILE":         bc.OpenAIKeyFile,
		"VOLC_ASR_API_KEY_FILE":       bc.VolcASRKeyFile,
		"AURORA_TASK_TOKEN_FILE":      bc.TaskTokenPath,
	}
	for name, value := range proxyEnv {
		env[name] = value
	}
	return env
}

// auroraBrokerMcpConfig builds the fixed Claude MCP config for the reviewed
// broker. It fails closed on any missing input: a partially-resolved context
// must fail the task, never launch with an unknown command, root, or secret.
// proxyEnv is the node's reviewed egress-proxy environment (may be nil); it is
// merged into the otherwise-fixed env allowlist.
func auroraBrokerMcpConfig(bc auroraBrokerContext, proxyEnv map[string]string) (json.RawMessage, error) {
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
		auroraBrokerServerName: {
			Command: "node",
			Args:    []string{auroraBrokerEntrypoint},
			// Exactly the compiled broker env allowlist. Adding any other key
			// (HOME, PATH, provider credentials) would widen the child's view
			// beyond the reviewed surface; the egress-proxy names are the one
			// reviewed addition (see auroraBrokerProxyEnv).
			Env: auroraBrokerEnv(bc, proxyEnv),
		},
	}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal broker MCP config: %v", errAuroraBrokerContextInvalid, err)
	}
	return raw, nil
}

// writeAuroraBrokerContext stages the task's inputs into the broker input root,
// writes the broker's mode-0400 context JSON and the mode-0400 task token under
// env.WorkDir, then returns the resolved context the MCP config is built from.
// It is fail-closed: every missing or non-UUID identity, absent token, unknown
// skill, or unstaged attachment is an error the caller turns into a task
// failure (the existing refund path), so the broker is never handed a context
// its own loadTaskContext would reject.
//
// generation_id is the real Aurora generation id the claim payload carries. It
// is never replaced with the task id: Aurora's completion and artifact routes
// key on the generation, so a placeholder would make the context lie.
func (d *Daemon) writeAuroraBrokerContext(ctx context.Context, task Task, env execenv.Environment) (auroraBrokerContext, error) {
	skillID, ok := auroraSkillID(task)
	if !ok {
		return auroraBrokerContext{}, fmt.Errorf("%w: no trusted skill id", errAuroraBrokerContextInvalid)
	}
	policy, ok := aurora.ExecutionPolicy(skillID)
	if !ok {
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
	if strings.TrimSpace(task.GenerationID) == "" {
		return auroraBrokerContext{}, fmt.Errorf("%w: task generation id is required", errAuroraBrokerContextInvalid)
	}
	generationID, err := auroraBrokerUUID(task.GenerationID, "generation id")
	if err != nil {
		return auroraBrokerContext{}, err
	}
	prompt := task.QuickCreatePrompt
	if strings.TrimSpace(prompt) == "" {
		return auroraBrokerContext{}, fmt.Errorf("%w: task prompt is required", errAuroraBrokerContextInvalid)
	}

	inputRoot := d.auroraBrokerInputRootPath()
	attachments, err := d.stageAuroraBrokerAttachments(ctx, task, policy, inputRoot)
	if err != nil {
		return auroraBrokerContext{}, err
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
		"attachments":     attachments,
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
		InputRoot:      inputRoot,
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

// stagedAttachment is one attachment the daemon has already classified and
// written, kept only so the skill-policy check runs over the exact facts the
// broker will read.
type stagedAttachment struct {
	kind aurora.AttachmentKind
	size int64
}

// stageAuroraBrokerAttachments downloads every quick-create attachment into the
// broker input root and returns the exact attachments map loadTaskContext
// accepts. It fails closed on any gap — an unresolvable id, a filename outside
// the broker's extension table, a download error, an over-cap file, or a set
// the skill's attachment policy rejects — so runTask aborts before launch
// instead of handing the broker a context it will refuse.
func (d *Daemon) stageAuroraBrokerAttachments(ctx context.Context, task Task, policy aurora.SkillExecutionPolicy, inputRoot string) (map[string]auroraBrokerAttachment, error) {
	requested := task.QuickCreateAttachmentIDs
	attachments := make(map[string]auroraBrokerAttachment, len(requested))
	staged := make([]stagedAttachment, 0, len(requested))
	if len(requested) == 0 {
		if err := validateAuroraBrokerAttachmentPolicy(policy, staged); err != nil {
			return nil, fmt.Errorf("%w: %v", errAuroraBrokerContextInvalid, err)
		}
		return attachments, nil
	}
	if d.client == nil {
		return nil, fmt.Errorf("%w: attachment staging requires the daemon client", errAuroraBrokerContextInvalid)
	}

	token := strings.TrimSpace(task.AuthToken)
	if err := os.MkdirAll(inputRoot, 0o700); err != nil {
		return nil, fmt.Errorf("%w: create broker input root: %v", errAuroraBrokerContextInvalid, err)
	}

	for _, raw := range requested {
		id, err := auroraBrokerUUID(raw, "attachment id")
		if err != nil {
			return nil, err
		}
		if _, duplicate := attachments[id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate attachment id %s", errAuroraBrokerContextInvalid, id)
		}
		meta, err := d.client.GetAuroraAttachment(ctx, token, id)
		if err != nil {
			return nil, fmt.Errorf("%w: load attachment %s: %v", errAuroraBrokerContextInvalid, id, err)
		}
		extension := strings.ToLower(filepath.Ext(strings.TrimSpace(meta.Filename)))
		format, ok := auroraBrokerAttachmentTypes[extension]
		if !ok {
			return nil, fmt.Errorf("%w: attachment %s has an unsupported extension %q", errAuroraBrokerContextInvalid, id, extension)
		}
		maxBytes, ok := auroraBrokerAttachmentMaxBytes(format.kind)
		if !ok {
			return nil, fmt.Errorf("%w: attachment %s has an unsupported kind", errAuroraBrokerContextInvalid, id)
		}
		if meta.SizeBytes > maxBytes {
			return nil, fmt.Errorf("%w: attachment %s exceeds the broker size cap", errAuroraBrokerContextInvalid, id)
		}
		data, err := d.client.DownloadAuroraAttachment(ctx, token, meta.DownloadURL, maxBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: download attachment %s: %v", errAuroraBrokerContextInvalid, id, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("%w: attachment %s staged empty", errAuroraBrokerContextInvalid, id)
		}
		if int64(len(data)) > maxBytes {
			return nil, fmt.Errorf("%w: attachment %s exceeds the broker size cap", errAuroraBrokerContextInvalid, id)
		}

		// The id is a canonical UUID and the extension comes from the compiled
		// table above, so the relative path is daemon-owned; the containment
		// check is the defensive assertion that the broker's own path rule
		// cannot fail.
		relative := id + extension
		target := filepath.Join(inputRoot, relative)
		if filepath.Dir(target) != filepath.Clean(inputRoot) {
			return nil, fmt.Errorf("%w: attachment %s path escapes the input root", errAuroraBrokerContextInvalid, id)
		}
		if err := writeAuroraInputFile(target, data); err != nil {
			return nil, fmt.Errorf("%w: stage attachment %s: %v", errAuroraBrokerContextInvalid, id, err)
		}
		attachments[id] = auroraBrokerAttachment{
			RelativePath: relative,
			MIMEType:     format.mimeType,
			SizeBytes:    int64(len(data)),
		}
		staged = append(staged, stagedAttachment{kind: format.kind, size: int64(len(data))})
	}

	if err := validateAuroraBrokerAttachmentPolicy(policy, staged); err != nil {
		return nil, fmt.Errorf("%w: %v", errAuroraBrokerContextInvalid, err)
	}
	return attachments, nil
}

// writeAuroraInputFile replaces any leftover input with the exact 0600 bytes.
// A prior task's run under a reused input root must never survive as a
// same-named file the broker would read.
func writeAuroraInputFile(path string, data []byte) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// validateAuroraBrokerAttachmentPolicy mirrors aurora.ValidateSkillInputs over
// the daemon's staged facts: every staged kind must belong to a constraint,
// every constraint's min/max must hold, and the per-constraint byte cap must not
// be exceeded. The daemon cannot read the attachment rows, so it re-derives the
// same decision from what it actually staged.
func validateAuroraBrokerAttachmentPolicy(policy aurora.SkillExecutionPolicy, staged []stagedAttachment) error {
	counts := make([]int, len(policy.Attachments))
	for _, file := range staged {
		index := -1
		for j, constraint := range policy.Attachments {
			if slices.Contains(constraint.Kinds, file.kind) {
				index = j
				break
			}
		}
		if index < 0 {
			return fmt.Errorf("%s files are not accepted by %s", file.kind, policy.SkillID)
		}
		if max := policy.Attachments[index].MaxBytes; max > 0 && file.size > max {
			return fmt.Errorf("attachment exceeds the %d byte cap for %s", max, policy.SkillID)
		}
		counts[index]++
	}
	for i, constraint := range policy.Attachments {
		if counts[i] < constraint.Min {
			return fmt.Errorf("%s requires at least %d %s file(s), got %d", policy.SkillID, constraint.Min, constraint.Kinds, counts[i])
		}
		if counts[i] > constraint.Max {
			return fmt.Errorf("%s accepts at most %d %s file(s), got %d", policy.SkillID, constraint.Max, constraint.Kinds, counts[i])
		}
	}
	return nil
}
