package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Fixed runtime secret destinations inside the managed sandbox image. The plan
// locks these paths; the fleet mounts read-only host files at exactly these
// destinations and the daemon never accepts a caller-supplied path.
const (
	managedAnthropicAPIKeyPath = "/run/secrets/anthropic-api-key"
	managedArkAPIKeyPath       = "/run/secrets/ark-api-key"
	managedOpenAIAPIKeyPath    = "/run/secrets/openai-api-key"
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
)

// managedSecretPaths are the host-visible paths of the fixed provider secret
// files. They are process configuration owned by the operator; the fleet
// control API can never supply or override them.
type managedSecretPaths struct {
	AnthropicAPIKey string
	ArkAPIKey       string
	OpenAIAPIKey    string
	VolcASRAPIKey   string
}

// defaultManagedSecretPaths returns the fixed in-image destinations.
func defaultManagedSecretPaths() managedSecretPaths {
	return managedSecretPaths{
		AnthropicAPIKey: managedAnthropicAPIKeyPath,
		ArkAPIKey:       managedArkAPIKeyPath,
		OpenAIAPIKey:    managedOpenAIAPIKeyPath,
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
		OpenAIAPIKey:    envOrDefault("OPENAI_API_KEY_FILE", defaults.OpenAIAPIKey),
		VolcASRAPIKey:   envOrDefault("VOLC_ASR_API_KEY_FILE", defaults.VolcASRAPIKey),
	}
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

// managedProviderSecrets holds the validated managed provider credentials. The
// Anthropic value is a secret value scoped to the Claude child; the other three
// are read-only file paths the MCP broker reads itself, so no provider value
// ever enters the daemon or the model-visible context.
type managedProviderSecrets struct {
	AnthropicAPIKey   managedSecret
	ArkAPIKeyFile     string
	OpenAIAPIKeyFile  string
	VolcASRAPIKeyFile string
}

// claudeChildEnv returns the environment additions for the Claude child
// process only: the single Anthropic credential value.
func (s managedProviderSecrets) claudeChildEnv() map[string]string {
	return map[string]string{"ANTHROPIC_API_KEY": s.AnthropicAPIKey.Value()}
}

// mcpBrokerChildEnv returns the environment additions for the MCP broker only:
// the three fixed provider file paths, never the values.
func (s managedProviderSecrets) mcpBrokerChildEnv() map[string]string {
	return map[string]string{
		"ARK_API_KEY_FILE":      s.ArkAPIKeyFile,
		"OPENAI_API_KEY_FILE":   s.OpenAIAPIKeyFile,
		"VOLC_ASR_API_KEY_FILE": s.VolcASRAPIKeyFile,
	}
}

// String redacts the whole set so fmt never prints a value or a path.
func (s managedProviderSecrets) String() string { return "[managed provider secrets redacted]" }

// LogValue records presence only.
func (s managedProviderSecrets) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("anthropic_api_key", s.AnthropicAPIKey.Value() != ""),
		slog.Bool("ark_api_key_file", s.ArkAPIKeyFile != ""),
		slog.Bool("openai_api_key_file", s.OpenAIAPIKeyFile != ""),
		slog.Bool("volc_asr_api_key_file", s.VolcASRAPIKeyFile != ""),
	)
}

// MarshalJSON redacts the Anthropic value; the file paths are not secret.
func (s managedProviderSecrets) MarshalJSON() ([]byte, error) {
	type wire struct {
		AnthropicAPIKey   string `json:"anthropic_api_key"`
		ArkAPIKeyFile     string `json:"ark_api_key_file"`
		OpenAIAPIKeyFile  string `json:"openai_api_key_file"`
		VolcASRAPIKeyFile string `json:"volc_asr_api_key_file"`
	}
	return json.Marshal(wire{
		AnthropicAPIKey:   s.AnthropicAPIKey.String(),
		ArkAPIKeyFile:     s.ArkAPIKeyFile,
		OpenAIAPIKeyFile:  s.OpenAIAPIKeyFile,
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

// loadManagedProviderSecrets validates every fixed provider credential file.
// All four are required because the sandbox advertises all thirteen skills; a
// missing file returns a provider-specific error that prints neither the path
// nor the value.
func loadManagedProviderSecrets(paths managedSecretPaths) (managedProviderSecrets, error) {
	read := func(provider, path string) (string, error) {
		value, err := readManagedSecretFile(path, managedSecretMaxBytes)
		if err != nil {
			return "", fmt.Errorf("managed mode requires the %s provider credential: %w", provider, err)
		}
		return value, nil
	}
	anthropic, err := read("anthropic", paths.AnthropicAPIKey)
	if err != nil {
		return managedProviderSecrets{}, err
	}
	if _, err := read("ark", paths.ArkAPIKey); err != nil {
		return managedProviderSecrets{}, err
	}
	if _, err := read("openai", paths.OpenAIAPIKey); err != nil {
		return managedProviderSecrets{}, err
	}
	if _, err := read("volc-asr", paths.VolcASRAPIKey); err != nil {
		return managedProviderSecrets{}, err
	}
	return managedProviderSecrets{
		AnthropicAPIKey:   managedSecret{value: anthropic},
		ArkAPIKeyFile:     paths.ArkAPIKey,
		OpenAIAPIKeyFile:  paths.OpenAIAPIKey,
		VolcASRAPIKeyFile: paths.VolcASRAPIKey,
	}, nil
}
