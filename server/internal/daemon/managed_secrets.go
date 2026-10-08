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

// Environment variable names the managed node carries its provider credentials
// in. The Fleet writes them into the node container env at deploy time (the
// Fleet config's claude_env, stored in an owner-only file), and "fleet-node
// run" hands its own environment to the managed daemon unchanged. The values
// are therefore readable by every process in the container and by docker
// inspect; that is the posture this direction explicitly accepts.
const (
	// managedAnthropicAPIKeyEnvName is the credential the managed Claude child
	// authenticates with. The Aurora plan reuses it as the Volcengine Ark Agent
	// Plan key: ARK's key is Anthropic-Messages-compatible, so the image, video
	// and text skills authenticate with this one value instead of a second
	// variable (decision 5).
	managedAnthropicAPIKeyEnvName = "ANTHROPIC_API_KEY"
	// managedVolcASRAPIKeyEnvName is the Volcengine speech credential. The
	// speech endpoint is the only provider that is not Anthropic-compatible, so
	// it is the single separate provider variable.
	managedVolcASRAPIKeyEnvName = "VOLC_ASR_API_KEY"
)

// Managed provider environment errors. They deliberately never name the value,
// so a startup failure cannot leak a credential into logs or an error surface.
var (
	errManagedEnvValueEmpty = errors.New("environment credential must not be empty")
	errManagedEnvMissing    = errors.New("environment credential is missing")
)

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
	if parsed.Host == "" || parsed.Hostname() == "" || strings.ContainsAny(parsed.Host, ", 	") {
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
// Both credentials are values read from the node's own environment at startup;
// no credential file and no read-only secret mount remains. The Anthropic value
// is both the credential the model authenticates with and the one an Aurora
// skill's shell steps call the Ark provider with (decision 5).
type managedProviderSecrets struct {
	AnthropicAPIKey  managedSecret
	AnthropicBaseURL string
	AnthropicModel   string
	VolcASRAPIKey    managedSecret
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
	env := map[string]string{managedAnthropicAPIKeyEnvName: s.AnthropicAPIKey.Value()}
	if s.AnthropicBaseURL != "" {
		env["ANTHROPIC_BASE_URL"] = s.AnthropicBaseURL
	}
	if s.AnthropicModel != "" {
		env["ANTHROPIC_MODEL"] = s.AnthropicModel
	}
	return env
}

// agentChildEnv returns the extra provider values the ordinary agent child's
// own shell steps read. The Volcengine speech key is the only one: the Ark key
// is already present as ANTHROPIC_API_KEY in claudeChildEnv, so an image or
// video step authenticates with that same variable (decision 5). An absent ASR
// key adds nothing; the route that needs it fails closed when it is invoked.
func (s managedProviderSecrets) agentChildEnv() map[string]string {
	if s.VolcASRAPIKey.Value() == "" {
		return nil
	}
	return map[string]string{managedVolcASRAPIKeyEnvName: s.VolcASRAPIKey.Value()}
}

// String redacts the whole set so fmt never prints a value.
func (s managedProviderSecrets) String() string { return "[managed provider secrets redacted]" }

// LogValue records presence only.
func (s managedProviderSecrets) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("anthropic_api_key", s.AnthropicAPIKey.Value() != ""),
		slog.Bool("anthropic_base_url", s.AnthropicBaseURL != ""),
		slog.Bool("anthropic_model", s.AnthropicModel != ""),
		slog.Bool("volc_asr_api_key", s.VolcASRAPIKey.Value() != ""),
	)
}

// MarshalJSON redacts both credential values; the endpoint overrides are not
// secret.
func (s managedProviderSecrets) MarshalJSON() ([]byte, error) {
	type wire struct {
		AnthropicAPIKey  string `json:"anthropic_api_key"`
		AnthropicBaseURL string `json:"anthropic_base_url"`
		AnthropicModel   string `json:"anthropic_model"`
		VolcASRAPIKey    string `json:"volc_asr_api_key"`
	}
	return json.Marshal(wire{
		AnthropicAPIKey:  s.AnthropicAPIKey.String(),
		AnthropicBaseURL: s.AnthropicBaseURL,
		AnthropicModel:   s.AnthropicModel,
		VolcASRAPIKey:    s.VolcASRAPIKey.String(),
	})
}

// readManagedProviderEnvValue reads one provider credential from the node's own
// environment. An unset variable is absent - the route that needs it fails
// closed at call time - while a variable that is set and empty is an operator
// typo and stops the load instead of shipping an empty credential.
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
// cannot start without its own credential, and that same credential is what an
// Aurora skill's shell steps call the Ark provider with (decision 5). The
// Volcengine speech key is read lazily, so an absent variable is tolerated -
// the route that needs it fails closed when it is invoked - while a set-but-
// empty value is rejected. Every error names only the provider, never the value.
func loadManagedProviderSecrets(endpoint managedClaudeEndpoint) (managedProviderSecrets, error) {
	anthropic, err := readManagedProviderEnvValue(managedAnthropicAPIKeyEnvName)
	if err != nil {
		return managedProviderSecrets{}, fmt.Errorf("managed mode requires the anthropic provider credential: %w", err)
	}
	if anthropic == "" {
		return managedProviderSecrets{}, fmt.Errorf("managed mode requires the anthropic provider credential: %w", errManagedEnvMissing)
	}
	volcASR, err := readManagedProviderEnvValue(managedVolcASRAPIKeyEnvName)
	if err != nil {
		return managedProviderSecrets{}, fmt.Errorf("managed mode provider credential volc-asr is invalid: %w", err)
	}
	return managedProviderSecrets{
		AnthropicAPIKey:  managedSecret{value: anthropic},
		AnthropicBaseURL: endpoint.BaseURL,
		AnthropicModel:   endpoint.Model,
		VolcASRAPIKey:    managedSecret{value: volcASR},
	}, nil
}
