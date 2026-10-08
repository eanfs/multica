package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
)

// Fixed runtime secret destinations inside the managed sandbox image. The plan
// locks these paths; the fleet mounts read-only host files at exactly these
// destinations and the daemon never accepts a caller-supplied path.
const (
	managedAnthropicAPIKeyPath = "/run/secrets/anthropic-api-key"
	managedArkAPIKeyPath       = "/run/secrets/ark-api-key"
	managedVolcASRAPIKeyPath   = "/run/secrets/volc-asr-api-key"
)

// managedSecretMaxBytes bounds any managed provider secret file so a
// compromised mount cannot feed the daemon an unbounded blob.
const managedSecretMaxBytes = 16 * 1024

// Managed provider secret file errors. They deliberately never name the path or
// the value so a startup failure cannot leak either into logs or an error
// surface.
var (
	errManagedSecretMissing     = errors.New("secret file is missing")
	errManagedSecretSymlink     = errors.New("secret file must not be a symlink")
	errManagedSecretNotRegular  = errors.New("secret file must be a regular file")
	errManagedSecretPermissions = errors.New("secret file must not be group- or world-accessible")
	errManagedSecretNotReadable = errors.New("secret file must be owner-readable")
	errManagedSecretTooLarge    = errors.New("secret file exceeds the size limit")
	errManagedSecretEmpty       = errors.New("secret file must not be empty")
	errManagedEnvValueEmpty     = errors.New("environment credential must not be empty")
)

// managedSecretPaths are the host-visible paths of the fixed provider secret
// files. They are process configuration owned by the operator; the fleet
// control API can never supply or override them.
type managedSecretPaths struct {
	AnthropicAPIKey string
	ArkAPIKey       string
	VolcASRAPIKey   string
}

// defaultManagedSecretPaths returns the fixed in-image destinations.
func defaultManagedSecretPaths() managedSecretPaths {
	return managedSecretPaths{
		AnthropicAPIKey: managedAnthropicAPIKeyPath,
		ArkAPIKey:       managedArkAPIKeyPath,
		VolcASRAPIKey:   managedVolcASRAPIKeyPath,
	}
}

// managedSecretPathsFromEnv layers the documented *_API_KEY_FILE overrides on
// top of the fixed destinations. The variable carries a path, never a value.
func managedSecretPathsFromEnv() managedSecretPaths {
	defaults := defaultManagedSecretPaths()
	return managedSecretPaths{
		AnthropicAPIKey: envOrDefault("ANTHROPIC_API_KEY_FILE", defaults.AnthropicAPIKey),
		ArkAPIKey:       envOrDefault("ARK_API_KEY_FILE", defaults.ArkAPIKey),
		VolcASRAPIKey:   envOrDefault("VOLC_ASR_API_KEY_FILE", defaults.VolcASRAPIKey),
	}
}

// managedClaudeEndpoint is the optional operator-configured Anthropic-compatible
// endpoint for the managed Claude child. Both fields are process configuration
// owned by the operator; the fleet control API can never supply or override
// them. Neither value is a secret.
type managedClaudeEndpoint struct {
	BaseURL string
	Model   string
}

// Managed Anthropic endpoint validation errors. They deliberately never echo
// the configured value so a startup failure cannot leak operator configuration
// into logs or an error surface.
var (
	errManagedBaseURLInvalid  = errors.New("managed ANTHROPIC_BASE_URL is not an absolute URL")
	errManagedBaseURLScheme   = errors.New("managed ANTHROPIC_BASE_URL must use the https scheme")
	errManagedBaseURLHost     = errors.New("managed ANTHROPIC_BASE_URL must name exactly one host")
	errManagedBaseURLUserInfo = errors.New("managed ANTHROPIC_BASE_URL must not embed credentials")
	errManagedBaseURLQuery    = errors.New("managed ANTHROPIC_BASE_URL must not include a query string")
	errManagedBaseURLFragment = errors.New("managed ANTHROPIC_BASE_URL must not include a fragment")
)

// managedClaudeEndpointFromEnv reads the optional ANTHROPIC_BASE_URL and
// ANTHROPIC_MODEL overrides for the managed Claude child. Unset (or empty)
// preserves the provider default. A supplied base URL must be a single https
// host with no embedded credentials, query, or fragment; an invalid value fails
// startup, and the error never echoes it.
func managedClaudeEndpointFromEnv() (managedClaudeEndpoint, error) {
	endpoint := managedClaudeEndpoint{
		BaseURL: strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
		Model:   strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL")),
	}
	if endpoint.BaseURL != "" {
		if err := validateManagedAnthropicBaseURL(endpoint.BaseURL); err != nil {
			return managedClaudeEndpoint{}, err
		}
	}
	return endpoint, nil
}

// validateManagedAnthropicBaseURL fails closed on any base URL that is not a
// single https host. It is intentionally narrower than the generic agent path,
// which forwards an operator-supplied value verbatim: the managed child runs in
// a fixed sandbox, so its egress target must be unambiguous.
func validateManagedAnthropicBaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() {
		return errManagedBaseURLInvalid
	}
	if parsed.Scheme != "https" {
		return errManagedBaseURLScheme
	}
	if parsed.User != nil {
		return errManagedBaseURLUserInfo
	}
	if parsed.Host == "" || parsed.Hostname() == "" || strings.ContainsAny(parsed.Host, ", \t") {
		return errManagedBaseURLHost
	}
	if parsed.RawQuery != "" {
		return errManagedBaseURLQuery
	}
	if parsed.Fragment != "" {
		return errManagedBaseURLFragment
	}
	return nil
}

// managedSecret is a credential value that never renders in logs, formatting,
// or JSON.
type managedSecret struct {
	value string
}

func (s managedSecret) Value() string { return s.value }

func (s managedSecret) String() string { return "[redacted]" }

func (s managedSecret) GoString() string { return "[redacted]" }

func (s managedSecret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

func (s managedSecret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// managedProviderSecrets holds the validated managed provider configuration.
// The Anthropic value is a secret value scoped to the Claude child, together
// with the optional operator endpoint overrides. ArkAPIKey is the value the
// ordinary agent child's own shell steps call Seedream with. The two remaining
// fields are the read-only file paths the MCP broker still reads itself while
// it exists.
type managedProviderSecrets struct {
	AnthropicAPIKey   managedSecret
	AnthropicBaseURL  string
	AnthropicModel    string
	ArkAPIKey         managedSecret
	ArkAPIKeyFile     string
	VolcASRAPIKeyFile string
}

// claudeChildEnv returns the environment additions for the Claude child process
// only: the Anthropic credential value plus the operator endpoint overrides
// when configured. When neither override is set the map is byte-for-byte the
// historical {ANTHROPIC_API_KEY}.
//
// The operator model is forwarded verbatim, never lowercased or stripped.
// Verified against Claude Code 2.1.289: for a model id outside its built-in
// registry (any ARK Agent Plan model) Claude Code emits one
// "[claude-code:unrecognized_model]" diagnostic naming its own lowercased
// canonical key, so "[1M]" prints as "[1m]". That key never reaches the wire:
// the Messages request keeps the configured base name with its original case
// and moves a trailing "[1M]" context marker into the context-1m beta header,
// and the ARK endpoint answers 200 for both spellings. The diagnostic is
// therefore cosmetic; normalizing the value here would not silence it. The
// Claude backend filters that one diagnostic (claude_stderr.go) only when a
// custom endpoint is configured, so the daemon does not paper over a
// first-party misconfiguration.
func (s managedProviderSecrets) claudeChildEnv() map[string]string {
	env := map[string]string{"ANTHROPIC_API_KEY": s.AnthropicAPIKey.Value()}
	if s.AnthropicBaseURL != "" {
		env["ANTHROPIC_BASE_URL"] = s.AnthropicBaseURL
	}
	if s.AnthropicModel != "" {
		env["ANTHROPIC_MODEL"] = s.AnthropicModel
	}
	return env
}

// agentChildEnv returns the environment additions for the ordinary agent
// child: the provider credential VALUES the model's own shell steps call the
// provider with. A *_API_KEY_FILE path is deliberately absent — a model running
// `curl -H 'Authorization: Bearer $ARK_API_KEY'` cannot dereference a path,
// and the broker's file view stays in mcpBrokerChildEnv. ARK is wired here
// first because the first executable image skill needs it; the Volcengine ASR
// key follows in the change that retires the broker.
func (s managedProviderSecrets) agentChildEnv() map[string]string {
	if s.ArkAPIKey.Value() == "" {
		return nil
	}
	return map[string]string{managedArkAPIKeyEnvName: s.ArkAPIKey.Value()}
}

// mcpBrokerChildEnv returns the broker's file-path view, never the values. The
// daemon no longer merges it into the agent environment: the broker is a Claude
// stdio child and receives these names through its own fixed MCP config env
// (auroraBrokerEnv). It stays for that contract's tests until the broker itself
// is deleted.
func (s managedProviderSecrets) mcpBrokerChildEnv() map[string]string {
	return map[string]string{
		"ARK_API_KEY_FILE":      s.ArkAPIKeyFile,
		"VOLC_ASR_API_KEY_FILE": s.VolcASRAPIKeyFile,
	}
}

// String redacts the whole set so fmt never prints a value or a path.
func (s managedProviderSecrets) String() string { return "[managed provider secrets redacted]" }

// LogValue records presence only.
func (s managedProviderSecrets) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("anthropic_api_key", s.AnthropicAPIKey.Value() != ""),
		slog.Bool("anthropic_base_url", s.AnthropicBaseURL != ""),
		slog.Bool("anthropic_model", s.AnthropicModel != ""),
		slog.Bool("ark_api_key", s.ArkAPIKey.Value() != ""),
		slog.Bool("ark_api_key_file", s.ArkAPIKeyFile != ""),
		slog.Bool("volc_asr_api_key_file", s.VolcASRAPIKeyFile != ""),
	)
}

// MarshalJSON redacts the secret values; the endpoint overrides and file paths
// are not secret.
func (s managedProviderSecrets) MarshalJSON() ([]byte, error) {
	type wire struct {
		AnthropicAPIKey   string `json:"anthropic_api_key"`
		AnthropicBaseURL  string `json:"anthropic_base_url"`
		AnthropicModel    string `json:"anthropic_model"`
		ArkAPIKey         string `json:"ark_api_key"`
		ArkAPIKeyFile     string `json:"ark_api_key_file"`
		VolcASRAPIKeyFile string `json:"volc_asr_api_key_file"`
	}
	return json.Marshal(wire{
		AnthropicAPIKey:   s.AnthropicAPIKey.String(),
		AnthropicBaseURL:  s.AnthropicBaseURL,
		AnthropicModel:    s.AnthropicModel,
		ArkAPIKey:         s.ArkAPIKey.String(),
		ArkAPIKeyFile:     s.ArkAPIKeyFile,
		VolcASRAPIKeyFile: s.VolcASRAPIKeyFile,
	})
}

// readManagedSecretFile reads one owner-only credential file. It fails closed
// on any path, permission, size, or shape surprise and never echoes the path or
// the value in its error.
func readManagedSecretFile(path string, maxBytes int64) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errManagedSecretMissing
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errManagedSecretMissing
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errManagedSecretSymlink
	}
	if !info.Mode().IsRegular() {
		return "", errManagedSecretNotRegular
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errManagedSecretPermissions
	}
	if info.Mode().Perm()&0o400 == 0 {
		return "", errManagedSecretNotReadable
	}
	if info.Size() > maxBytes {
		return "", errManagedSecretTooLarge
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errManagedSecretMissing
	}
	if int64(len(data)) > maxBytes {
		return "", errManagedSecretTooLarge
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errManagedSecretEmpty
	}
	return value, nil
}

// managedArkAPIKeyEnvName is the node environment variable that carries the Ark
// provider credential as a value. The Fleet node injects it at deploy time; the
// daemon reads it from its own process environment and forwards it to the
// ordinary agent child, whose skill documents name the same variable.
const managedArkAPIKeyEnvName = "ARK_API_KEY"

// readManagedProviderEnvValue reads one provider credential value from the
// node's own environment. An unset variable is absent — the route that needs it
// fails closed at call time — while a variable that is set and empty is an
// error, so an operator typo cannot silently ship an empty credential.
func readManagedProviderEnvValue(name string) (string, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return "", nil
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errManagedEnvValueEmpty
	}
	return value, nil
}

// loadManagedProviderSecrets validates the managed provider credentials and
// applies the already-validated Claude endpoint overrides. Only the Anthropic
// value is required at startup: the managed daemon is the Claude agent and
// cannot start without its own credential. The Ark credential is read from the
// node environment as a value for the ordinary agent child; an unset variable
// is tolerated because the routes that need it fail closed at call time, while
// a set-but-empty value is rejected. The two broker file paths are read lazily,
// so a missing file is tolerated — the tool that needs it fails closed when it
// is invoked — while a supplied file that is not a safe owner-only regular file
// still fails startup. Every error names only the provider, never the path or
// the value.
func loadManagedProviderSecrets(paths managedSecretPaths, endpoint managedClaudeEndpoint) (managedProviderSecrets, error) {
	anthropic, err := readManagedSecretFile(paths.AnthropicAPIKey, managedSecretMaxBytes)
	if err != nil {
		return managedProviderSecrets{}, fmt.Errorf("managed mode requires the anthropic provider credential: %w", err)
	}
	for _, optional := range []struct {
		provider string
		path     string
	}{
		{"ark", paths.ArkAPIKey},
		{"volc-asr", paths.VolcASRAPIKey},
	} {
		if _, err := readManagedSecretFile(optional.path, managedSecretMaxBytes); err != nil && !errors.Is(err, errManagedSecretMissing) {
			return managedProviderSecrets{}, fmt.Errorf("managed mode provider credential %s is invalid: %w", optional.provider, err)
		}
	}
	arkValue, err := readManagedProviderEnvValue(managedArkAPIKeyEnvName)
	if err != nil {
		return managedProviderSecrets{}, fmt.Errorf("managed mode provider credential ark is invalid: %w", err)
	}
	return managedProviderSecrets{
		AnthropicAPIKey:   managedSecret{value: anthropic},
		AnthropicBaseURL:  endpoint.BaseURL,
		AnthropicModel:    endpoint.Model,
		ArkAPIKey:         managedSecret{value: arkValue},
		ArkAPIKeyFile:     paths.ArkAPIKey,
		VolcASRAPIKeyFile: paths.VolcASRAPIKey,
	}, nil
}

// managedAgentCredentialEnv is the credential overlay the daemon merges into
// the assembled task environment of a managed node. Only the Aurora execution
// provider runs the reviewed Aurora contract, so only it receives the managed
// credentials. The ordinary agent child gets the provider values; the MCP
// broker, while it still exists, gets its file paths through its own fixed MCP
// config env (auroraBrokerEnv) instead of this overlay.
func managedAgentCredentialEnv(secrets managedProviderSecrets, provider string) map[string]string {
	if provider != auroraExecutionProvider {
		return nil
	}
	env := map[string]string{}
	for name, value := range secrets.agentChildEnv() {
		env[name] = value
	}
	for name, value := range secrets.claudeChildEnv() {
		env[name] = value
	}
	return env
}
