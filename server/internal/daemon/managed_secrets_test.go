package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testAnthropicSecret = "sk-ant-managed-test-anthropic-value"
	testVolcASRSecret   = "asr-managed-test-value"
)

// stageManagedProviderSecrets puts the two provider credentials the managed
// daemon reads into the process environment, the way the Fleet injects them at
// deploy time, and loads them through the production loader.
func stageManagedProviderSecrets(t *testing.T) managedProviderSecrets {
	t.Helper()
	t.Setenv(managedAnthropicAPIKeyEnvName, testAnthropicSecret)
	t.Setenv(managedVolcASRAPIKeyEnvName, testVolcASRSecret)
	secrets, err := loadManagedProviderSecrets(managedClaudeEndpoint{})
	if err != nil {
		t.Fatalf("loadManagedProviderSecrets: %v", err)
	}
	return secrets
}

// TestProviderSecretsReadTheAsrKeyFromTheEnvironment pins the deployment
// contract for the one provider credential that is not Anthropic-compatible:
// its value arrives in the node environment, and no *_API_KEY_FILE path exists
// any more.
func TestProviderSecretsReadTheAsrKeyFromTheEnvironment(t *testing.T) {
	t.Setenv(managedAnthropicAPIKeyEnvName, testAnthropicSecret)
	t.Setenv(managedVolcASRAPIKeyEnvName, "volc-from-env")

	secrets, err := loadManagedProviderSecrets(managedClaudeEndpoint{})
	if err != nil {
		t.Fatalf("loadManagedProviderSecrets: %v", err)
	}
	env := secrets.agentChildEnv()
	if env[managedVolcASRAPIKeyEnvName] != "volc-from-env" {
		t.Fatalf("agent child env = %v, want the Volcengine ASR value", env)
	}
	for name := range env {
		if strings.HasSuffix(name, "_API_KEY_FILE") || name == "ARK_API_KEY" {
			t.Fatalf("a retired provider variable survived: %v", env)
		}
	}
	claude := secrets.claudeChildEnv()
	if claude[managedAnthropicAPIKeyEnvName] != testAnthropicSecret {
		t.Fatalf("claude child env = %v, want the Anthropic value", claude)
	}
	if _, ok := claude[managedVolcASRAPIKeyEnvName]; ok {
		t.Fatalf("claude child env carries the ASR key: %v", claude)
	}
}

// TestProviderSecretsRejectMissingValues pins the fail-closed half: the
// required credential must be present, and a variable that is set but empty is
// an operator typo rather than an absent key.
func TestProviderSecretsRejectMissingValues(t *testing.T) {
	t.Run("anthropic absent", func(t *testing.T) {
		t.Setenv(managedAnthropicAPIKeyEnvName, "placeholder")
		if err := os.Unsetenv(managedAnthropicAPIKeyEnvName); err != nil {
			t.Fatalf("unset %s: %v", managedAnthropicAPIKeyEnvName, err)
		}
		_, err := loadManagedProviderSecrets(managedClaudeEndpoint{})
		if err == nil {
			t.Fatal("loadManagedProviderSecrets accepted a missing Anthropic credential")
		}
		if !strings.Contains(err.Error(), "anthropic") {
			t.Fatalf("error is not provider-specific: %v", err)
		}
	})

	t.Run("anthropic set but empty", func(t *testing.T) {
		t.Setenv(managedAnthropicAPIKeyEnvName, "   ")
		_, err := loadManagedProviderSecrets(managedClaudeEndpoint{})
		if err == nil {
			t.Fatal("loadManagedProviderSecrets accepted a set-but-empty Anthropic credential")
		}
		if !strings.Contains(err.Error(), "anthropic") {
			t.Fatalf("error is not provider-specific: %v", err)
		}
	})

	t.Run("asr set but empty", func(t *testing.T) {
		t.Setenv(managedAnthropicAPIKeyEnvName, testAnthropicSecret)
		t.Setenv(managedVolcASRAPIKeyEnvName, "  ")
		_, err := loadManagedProviderSecrets(managedClaudeEndpoint{})
		if err == nil {
			t.Fatal("loadManagedProviderSecrets accepted a set-but-empty ASR credential")
		}
		if !strings.Contains(err.Error(), "volc-asr") {
			t.Fatalf("error is not provider-specific: %v", err)
		}
		if strings.Contains(err.Error(), testAnthropicSecret) {
			t.Fatalf("error leaks a secret value: %v", err)
		}
	})
}

// TestManagedProviderEnvScoping proves each credential reaches only the child
// that needs it: the Anthropic value goes to the Claude child, the Volcengine
// ASR value to the ordinary agent child, and neither value reaches the other.
func TestManagedProviderEnvScoping(t *testing.T) {
	secrets := stageManagedProviderSecrets(t)

	claude := secrets.claudeChildEnv()
	if len(claude) != 1 {
		t.Fatalf("claude child env = %v, want only ANTHROPIC_API_KEY", claude)
	}
	if claude[managedAnthropicAPIKeyEnvName] != testAnthropicSecret {
		t.Fatalf("claude child env value = %q, want the anthropic secret", claude[managedAnthropicAPIKeyEnvName])
	}
	if _, ok := claude[managedVolcASRAPIKeyEnvName]; ok {
		t.Error("claude child env carries the ASR key")
	}

	agent := secrets.agentChildEnv()
	if len(agent) != 1 || agent[managedVolcASRAPIKeyEnvName] != testVolcASRSecret {
		t.Fatalf("agent child env = %v, want only the ASR value", agent)
	}
	for _, name := range []string{"ARK_API_KEY", "ANTHROPIC_API_KEY", "ARK_API_KEY_FILE", "VOLC_ASR_API_KEY_FILE"} {
		if _, ok := agent[name]; ok {
			t.Errorf("agent child env carries retired name %s", name)
		}
	}
}

// TestAgentChildEnvOmitsAnAbsentAsrKey pins the lazy half: a deployment without
// the speech credential adds nothing, and the transcription route fails closed
// at call time instead of shipping an empty value.
func TestAgentChildEnvOmitsAnAbsentAsrKey(t *testing.T) {
	t.Setenv(managedAnthropicAPIKeyEnvName, testAnthropicSecret)
	t.Setenv(managedVolcASRAPIKeyEnvName, "placeholder")
	if err := os.Unsetenv(managedVolcASRAPIKeyEnvName); err != nil {
		t.Fatalf("unset %s: %v", managedVolcASRAPIKeyEnvName, err)
	}
	secrets, err := loadManagedProviderSecrets(managedClaudeEndpoint{})
	if err != nil {
		t.Fatalf("loadManagedProviderSecrets: %v", err)
	}
	if env := secrets.agentChildEnv(); len(env) != 0 {
		t.Fatalf("agent child env = %v, want nothing with an absent ASR key", env)
	}
}

// managedClaudeTestOverrides prepares a minimal valid managed startup so a test
// can drive LoadConfig with its own ANTHROPIC_* environment. It stages the two
// provider credentials and returns the required overrides.
func managedClaudeTestOverrides(t *testing.T) Overrides {
	t.Helper()
	fakeClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("MULTICA_CLAUDE_PATH", fakeClaude)
	t.Setenv("MULTICA_DAEMON_ID", "")
	t.Setenv("MULTICA_LAUNCHED_BY", "")
	stageManagedProviderSecrets(t)
	return Overrides{
		Managed:                    true,
		ManagedEnrollmentTokenFile: writeManagedTokenFile(t, testManagedEnrollmentToken),
		Foreground:                 true,
		ServerURL:                  "http://localhost:0",
		WorkspacesRoot:             t.TempDir(),
	}
}

// TestManagedClaudeChildEnvUnsetKeepsOnlyCredential pins the default contract:
// when neither operator endpoint override is set, the Claude child still
// receives exactly its credential and nothing else.
func TestManagedClaudeChildEnvUnsetKeepsOnlyCredential(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	secrets := stageManagedProviderSecrets(t)

	claude := secrets.claudeChildEnv()
	if len(claude) != 1 {
		t.Fatalf("claude child env = %v, want only ANTHROPIC_API_KEY", claude)
	}
	if claude[managedAnthropicAPIKeyEnvName] != testAnthropicSecret {
		t.Fatalf("claude child env value = %q, want the anthropic secret", claude[managedAnthropicAPIKeyEnvName])
	}
	if _, ok := claude["ANTHROPIC_BASE_URL"]; ok {
		t.Error("claude child env carries ANTHROPIC_BASE_URL while it is unset")
	}
	if _, ok := claude["ANTHROPIC_MODEL"]; ok {
		t.Error("claude child env carries ANTHROPIC_MODEL while it is unset")
	}
}

// TestManagedClaudeConfigForwardsOperatorEndpoint proves the operator-set base
// URL and model reach the managed Claude child.
func TestManagedClaudeConfigForwardsOperatorEndpoint(t *testing.T) {
	const baseURL = "https://ark.cn-beijing.volces.com/api/plan"
	const model = "ark-code-latest"
	t.Setenv("ANTHROPIC_BASE_URL", baseURL)
	t.Setenv("ANTHROPIC_MODEL", model)

	cfg, err := LoadConfig(managedClaudeTestOverrides(t))
	if err != nil {
		t.Fatalf("LoadConfig(managed) = %v", err)
	}
	claude := cfg.Managed.ProviderSecrets.claudeChildEnv()
	if len(claude) != 3 {
		t.Fatalf("claude child env = %v, want the credential, base URL, and model", claude)
	}
	if claude[managedAnthropicAPIKeyEnvName] != testAnthropicSecret {
		t.Errorf("claude child env credential = %q, want the staged secret", claude[managedAnthropicAPIKeyEnvName])
	}
	if claude["ANTHROPIC_BASE_URL"] != baseURL {
		t.Errorf("claude child env base URL = %q, want %q", claude["ANTHROPIC_BASE_URL"], baseURL)
	}
	if claude["ANTHROPIC_MODEL"] != model {
		t.Errorf("claude child env model = %q, want %q", claude["ANTHROPIC_MODEL"], model)
	}
}

// TestManagedClaudeModelSurvivesVerbatim pins the invariant behind the
// cosmetic Claude Code `[claude-code:unrecognized_model]` diagnostic: the
// operator's ANTHROPIC_MODEL reaches the managed Claude child byte-for-byte,
// including letter case and a trailing `[1M]` context marker. Claude Code,
// not the daemon, decides how that string is displayed and sent (the wire keeps
// the configured case and moves the marker into the context-1m beta header),
// and both spellings answer 200 on the ARK Agent Plan endpoint, so the
// diagnostic is noise, not an error. The daemon must never normalize the value.
func TestManagedClaudeModelSurvivesVerbatim(t *testing.T) {
	const model = "GLM-5.3-Flash[1M]"
	t.Setenv("ANTHROPIC_BASE_URL", "https://ark.cn-beijing.volces.com/api/plan")
	t.Setenv("ANTHROPIC_MODEL", model)

	cfg, err := LoadConfig(managedClaudeTestOverrides(t))
	if err != nil {
		t.Fatalf("LoadConfig(managed) = %v", err)
	}
	claude := cfg.Managed.ProviderSecrets.claudeChildEnv()
	if claude["ANTHROPIC_MODEL"] != model {
		t.Errorf("claude child env model = %q, want the verbatim %q", claude["ANTHROPIC_MODEL"], model)
	}
}

// TestManagedClaudeConfigRejectsInvalidBaseURL pins the fail-closed startup
// contract for the managed base URL, and that the rejection never echoes the
// offending value.
func TestManagedClaudeConfigRejectsInvalidBaseURL(t *testing.T) {
	cases := []struct {
		name  string
		value string
		leak  string
	}{
		{"plain http", "http://ark.cn-beijing.volces.com/api/plan", ""},
		{"embedded credentials", "https://user:secret-pass@ark.cn-beijing.volces.com/api/plan", "secret-pass"},
		{"query string", "https://ark.cn-beijing.volces.com/api/plan?token=secret-query", "secret-query"},
		{"missing host", "https:///api/plan", ""},
		{"relative", "/api/plan", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_BASE_URL", tc.value)

			_, err := LoadConfig(managedClaudeTestOverrides(t))
			if err == nil {
				t.Fatalf("LoadConfig(managed) accepted an invalid ANTHROPIC_BASE_URL")
			}
			if !strings.Contains(err.Error(), "ANTHROPIC_BASE_URL") {
				t.Fatalf("error does not name the invalid setting: %v", err)
			}
			if strings.Contains(err.Error(), tc.value) {
				t.Fatalf("error leaks the configured value: %v", err)
			}
			if tc.leak != "" && strings.Contains(err.Error(), tc.leak) {
				t.Fatalf("error leaks a credential embedded in the value: %v", err)
			}
		})
	}
}

// TestManagedSecretRedaction proves the credential values cannot render through
// fmt, slog, or JSON.
func TestManagedSecretRedaction(t *testing.T) {
	secrets := stageManagedProviderSecrets(t)

	for _, rendered := range []string{fmt.Sprint(secrets), fmt.Sprintf("%+v", secrets)} {
		if strings.Contains(rendered, testAnthropicSecret) || strings.Contains(rendered, testVolcASRSecret) {
			t.Fatalf("fmt rendering leaks a provider secret: %s", rendered)
		}
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("managed secrets", "secrets", secrets)
	logged := buf.String()
	if strings.Contains(logged, testAnthropicSecret) || strings.Contains(logged, testVolcASRSecret) {
		t.Fatalf("log output leaks a provider secret: %s", logged)
	}

	encoded, err := json.Marshal(secrets)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(encoded), testAnthropicSecret) || strings.Contains(string(encoded), testVolcASRSecret) {
		t.Fatalf("json output leaks a provider secret: %s", encoded)
	}
}

// TestManagedSecretConfigLoadsProviderEnv proves managed startup loads both
// provider credentials from the process environment through LoadConfig.
func TestManagedSecretConfigLoadsProviderEnv(t *testing.T) {
	cfg, err := LoadConfig(managedClaudeTestOverrides(t))
	if err != nil {
		t.Fatalf("LoadConfig(managed) = %v", err)
	}
	if got := cfg.Managed.ProviderSecrets.AnthropicAPIKey.Value(); got != testAnthropicSecret {
		t.Errorf("loaded anthropic value = %q, want the staged secret", got)
	}
	if got := cfg.Managed.ProviderSecrets.VolcASRAPIKey.Value(); got != testVolcASRSecret {
		t.Errorf("loaded ASR value = %q, want the staged secret", got)
	}
}

// TestManagedSecretConfigReportsAnthropicFailure proves managed startup fails
// without the credential the Claude agent itself needs, without echoing the
// value.
func TestManagedSecretConfigReportsAnthropicFailure(t *testing.T) {
	fakeClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("MULTICA_CLAUDE_PATH", fakeClaude)
	t.Setenv("MULTICA_DAEMON_ID", "")
	t.Setenv("MULTICA_LAUNCHED_BY", "")
	t.Setenv(managedAnthropicAPIKeyEnvName, "placeholder")
	if err := os.Unsetenv(managedAnthropicAPIKeyEnvName); err != nil {
		t.Fatalf("unset %s: %v", managedAnthropicAPIKeyEnvName, err)
	}
	t.Setenv(managedVolcASRAPIKeyEnvName, testVolcASRSecret)

	_, err := LoadConfig(Overrides{
		Managed:                    true,
		ManagedEnrollmentTokenFile: writeManagedTokenFile(t, testManagedEnrollmentToken),
		Foreground:                 true,
		ServerURL:                  "http://localhost:0",
		WorkspacesRoot:             t.TempDir(),
	})
	if err == nil {
		t.Fatal("LoadConfig(managed) accepted a missing Anthropic credential")
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("error is not provider-specific: %v", err)
	}
	if strings.Contains(err.Error(), testVolcASRSecret) {
		t.Fatalf("error leaks a provider secret: %v", err)
	}
}

// TestManagedSecretConfigAllowsMissingOptionalProviders proves a single-route
// deployment starts when only the Claude credential is staged: the absent
// speech credential does not fail managed startup.
func TestManagedSecretConfigAllowsMissingOptionalProviders(t *testing.T) {
	fakeClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("MULTICA_CLAUDE_PATH", fakeClaude)
	t.Setenv("MULTICA_DAEMON_ID", "")
	t.Setenv("MULTICA_LAUNCHED_BY", "")
	t.Setenv(managedAnthropicAPIKeyEnvName, testAnthropicSecret)
	t.Setenv(managedVolcASRAPIKeyEnvName, "placeholder")
	if err := os.Unsetenv(managedVolcASRAPIKeyEnvName); err != nil {
		t.Fatalf("unset %s: %v", managedVolcASRAPIKeyEnvName, err)
	}

	cfg, err := LoadConfig(Overrides{
		Managed:                    true,
		ManagedEnrollmentTokenFile: writeManagedTokenFile(t, testManagedEnrollmentToken),
		Foreground:                 true,
		ServerURL:                  "http://localhost:0",
		WorkspacesRoot:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig(managed) with the optional provider absent: %v", err)
	}
	if got := cfg.Managed.ProviderSecrets.AnthropicAPIKey.Value(); got != testAnthropicSecret {
		t.Errorf("loaded anthropic value = %q, want the staged secret", got)
	}
}
