package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testAnthropicSecret = "sk-ant-managed-test-anthropic-value"
	testArkSecret       = "ark-managed-test-value"
	testVolcASRSecret   = "asr-managed-test-value"
)

// writeManagedSecretFile stages one owner-only secret file, creating its
// directory with the same restrictive mode the controller uses.
func writeManagedSecretFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write secret %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod secret %s: %v", path, err)
	}
}

// managedSecretTestPaths returns four distinct secret file paths in one temp
// directory without creating the files.
func managedSecretTestPaths(t *testing.T) managedSecretPaths {
	t.Helper()
	dir := t.TempDir()
	return managedSecretPaths{
		AnthropicAPIKey: filepath.Join(dir, "anthropic-api-key"),
		ArkAPIKey:       filepath.Join(dir, "ark-api-key"),
		VolcASRAPIKey:   filepath.Join(dir, "volc-asr-api-key"),
	}
}

// setManagedSecretPathEnv points the documented *_API_KEY_FILE overrides at
// the supplied paths.
func setManagedSecretPathEnv(t *testing.T, paths managedSecretPaths) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY_FILE", paths.AnthropicAPIKey)
	t.Setenv("ARK_API_KEY_FILE", paths.ArkAPIKey)
	t.Setenv("VOLC_ASR_API_KEY_FILE", paths.VolcASRAPIKey)
}

// stageManagedProviderSecrets writes the three valid provider secret files,
// points the env overrides at them, and loads them through the production
// loader.
func stageManagedProviderSecrets(t *testing.T) (managedSecretPaths, managedProviderSecrets) {
	t.Helper()
	paths := managedSecretTestPaths(t)
	writeManagedSecretFile(t, paths.AnthropicAPIKey, testAnthropicSecret+"\n", 0o400)
	writeManagedSecretFile(t, paths.ArkAPIKey, testArkSecret+"\n", 0o400)
	writeManagedSecretFile(t, paths.VolcASRAPIKey, testVolcASRSecret+"\n", 0o400)
	setManagedSecretPathEnv(t, paths)
	secrets, err := loadManagedProviderSecrets(paths, managedClaudeEndpoint{})
	if err != nil {
		t.Fatalf("loadManagedProviderSecrets: %v", err)
	}
	return paths, secrets
}

// TestManagedSecretFileValidation pins the fail-closed file contract shared by
// every managed credential: a regular, non-symlink, owner-only file of bounded
// size whose errors never echo the path or the secret.
func TestManagedSecretFileValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		writeManagedSecretFile(t, path, content, mode)
		return path
	}
	valid := write("valid", "value\n", 0o400)
	groupReadable := write("group-readable", "value", 0o440)
	worldReadable := write("world-readable", "value", 0o404)
	notOwnerReadable := write("not-owner-readable", "value", 0o000)
	oversized := write("oversized", strings.Repeat("a", managedSecretMaxBytes+1), 0o400)
	empty := write("empty", "", 0o400)
	symlinkTarget := write("symlink-target", "value", 0o400)
	symlink := filepath.Join(dir, "symlink")
	if err := os.Symlink(symlinkTarget, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	directory := filepath.Join(dir, "as-directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cases := []struct {
		name     string
		path     string
		sentinel error
	}{
		{"missing path", "", errManagedSecretMissing},
		{"absent file", filepath.Join(dir, "absent"), errManagedSecretMissing},
		{"directory", directory, errManagedSecretNotRegular},
		{"symlink", symlink, errManagedSecretSymlink},
		{"group readable", groupReadable, errManagedSecretPermissions},
		{"world readable", worldReadable, errManagedSecretPermissions},
		{"not owner readable", notOwnerReadable, errManagedSecretNotReadable},
		{"oversized", oversized, errManagedSecretTooLarge},
		{"empty", empty, errManagedSecretEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readManagedSecretFile(tc.path, managedSecretMaxBytes)
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("readManagedSecretFile(%s) = %v, want %v", tc.name, err, tc.sentinel)
			}
			if tc.path != "" && strings.Contains(err.Error(), tc.path) {
				t.Fatalf("error leaks the secret path: %v", err)
			}
		})
	}

	t.Run("newline trimmed", func(t *testing.T) {
		got, err := readManagedSecretFile(valid, managedSecretMaxBytes)
		if err != nil {
			t.Fatalf("readManagedSecretFile(valid) = %v", err)
		}
		if got != "value" {
			t.Fatalf("readManagedSecretFile(valid) = %q, want %q", got, "value")
		}
	})

	t.Run("error never leaks the value", func(t *testing.T) {
		leaky := write("leaky", "super-secret-leak-value", 0o644)
		_, err := readManagedSecretFile(leaky, managedSecretMaxBytes)
		if err == nil {
			t.Fatal("expected a permissions error")
		}
		if strings.Contains(err.Error(), "super-secret-leak-value") {
			t.Fatalf("error leaks the secret value: %v", err)
		}
	})
}

// TestManagedSecretLoaderRequiresOnlyAnthropic pins the startup requirement:
// the managed daemon is the Claude agent, so its Anthropic value is required,
// while the other three provider tools read their credential files lazily and
// fail closed at call time. Those three may therefore be absent at startup, and
// their configured paths are still handed to the MCP broker.
func TestManagedSecretLoaderRequiresOnlyAnthropic(t *testing.T) {
	t.Run("optional providers absent", func(t *testing.T) {
		paths := managedSecretTestPaths(t)
		writeManagedSecretFile(t, paths.AnthropicAPIKey, testAnthropicSecret, 0o400)

		secrets, err := loadManagedProviderSecrets(paths, managedClaudeEndpoint{})
		if err != nil {
			t.Fatalf("loadManagedProviderSecrets with optional files absent: %v", err)
		}
		if got := secrets.AnthropicAPIKey.Value(); got != testAnthropicSecret {
			t.Errorf("anthropic value = %q, want the staged secret", got)
		}
		if secrets.ArkAPIKeyFile != paths.ArkAPIKey ||
			secrets.VolcASRAPIKeyFile != paths.VolcASRAPIKey {
			t.Errorf("optional provider paths were not preserved: %+v", secrets)
		}
	})

	t.Run("anthropic absent", func(t *testing.T) {
		paths := managedSecretTestPaths(t)
		writeManagedSecretFile(t, paths.ArkAPIKey, testArkSecret, 0o400)

		_, err := loadManagedProviderSecrets(paths, managedClaudeEndpoint{})
		if err == nil {
			t.Fatal("loadManagedProviderSecrets accepted a missing Anthropic credential")
		}
		if !strings.Contains(err.Error(), "anthropic") {
			t.Fatalf("error is not provider-specific: %v", err)
		}
		if strings.Contains(err.Error(), paths.AnthropicAPIKey) {
			t.Fatalf("error leaks the missing path: %v", err)
		}
		if strings.Contains(err.Error(), testArkSecret) {
			t.Fatalf("error leaks a secret value: %v", err)
		}
	})
}

// TestManagedSecretLoaderRejectsUnsafeOptionalProvider keeps the file safety
// checks for provider files that are present: a supplied file that is not an
// owner-only regular file still fails startup.
func TestManagedSecretLoaderRejectsUnsafeOptionalProvider(t *testing.T) {
	paths := managedSecretTestPaths(t)
	writeManagedSecretFile(t, paths.AnthropicAPIKey, testAnthropicSecret, 0o400)
	writeManagedSecretFile(t, paths.ArkAPIKey, testArkSecret, 0o440)

	_, err := loadManagedProviderSecrets(paths, managedClaudeEndpoint{})
	if err == nil {
		t.Fatal("loadManagedProviderSecrets accepted a group-readable optional provider file")
	}
	if !strings.Contains(err.Error(), "ark") {
		t.Fatalf("error is not provider-specific: %v", err)
	}
	if strings.Contains(err.Error(), paths.ArkAPIKey) {
		t.Fatalf("error leaks the path: %v", err)
	}
}

// TestManagedSecretLoaderReadsFixedProviders pins the mapping from file to
// struct field and that only the Anthropic value is retained as a value.
func TestManagedSecretLoaderReadsFixedProviders(t *testing.T) {
	paths, secrets := stageManagedProviderSecrets(t)
	if got := secrets.AnthropicAPIKey.Value(); got != testAnthropicSecret {
		t.Errorf("anthropic value = %q, want %q", got, testAnthropicSecret)
	}
	if secrets.ArkAPIKeyFile != paths.ArkAPIKey {
		t.Errorf("ark file = %q, want %q", secrets.ArkAPIKeyFile, paths.ArkAPIKey)
	}
	if secrets.VolcASRAPIKeyFile != paths.VolcASRAPIKey {
		t.Errorf("asr file = %q, want %q", secrets.VolcASRAPIKeyFile, paths.VolcASRAPIKey)
	}
}

// TestManagedSecretChildEnvScoping proves the Anthropic value reaches only the
// Claude child, and the two provider file paths reach only the MCP broker.
func TestManagedSecretChildEnvScoping(t *testing.T) {
	_, secrets := stageManagedProviderSecrets(t)

	claude := secrets.claudeChildEnv()
	if len(claude) != 1 {
		t.Fatalf("claude child env = %v, want only ANTHROPIC_API_KEY", claude)
	}
	if claude["ANTHROPIC_API_KEY"] != testAnthropicSecret {
		t.Fatalf("claude child env value = %q, want the anthropic secret", claude["ANTHROPIC_API_KEY"])
	}
	for _, name := range []string{"ARK_API_KEY_FILE", "VOLC_ASR_API_KEY_FILE"} {
		if _, ok := claude[name]; ok {
			t.Errorf("claude child env carries broker name %s", name)
		}
	}
}

// managedClaudeTestOverrides prepares a minimal valid managed startup so a test
// can drive LoadConfig with its own ANTHROPIC_* environment. It stages the four
// provider secret files and returns the required overrides.
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
	_, secrets := stageManagedProviderSecrets(t)

	claude := secrets.claudeChildEnv()
	if len(claude) != 1 {
		t.Fatalf("claude child env = %v, want only ANTHROPIC_API_KEY", claude)
	}
	if claude["ANTHROPIC_API_KEY"] != testAnthropicSecret {
		t.Fatalf("claude child env value = %q, want the anthropic secret", claude["ANTHROPIC_API_KEY"])
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
	if claude["ANTHROPIC_API_KEY"] != testAnthropicSecret {
		t.Errorf("claude child env credential = %q, want the staged secret", claude["ANTHROPIC_API_KEY"])
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

// TestManagedSecretRedaction proves the credential value cannot render through
// fmt, slog, or JSON.
func TestManagedSecretRedaction(t *testing.T) {
	_, secrets := stageManagedProviderSecrets(t)

	for _, rendered := range []string{fmt.Sprint(secrets), fmt.Sprintf("%+v", secrets)} {
		if strings.Contains(rendered, testAnthropicSecret) {
			t.Fatalf("fmt rendering leaks the anthropic secret: %s", rendered)
		}
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("managed secrets", "secrets", secrets)
	logged := buf.String()
	if strings.Contains(logged, testAnthropicSecret) {
		t.Fatalf("log output leaks the anthropic secret: %s", logged)
	}
	if strings.Contains(logged, secrets.ArkAPIKeyFile) {
		t.Fatalf("log output leaks a provider secret path: %s", logged)
	}

	encoded, err := json.Marshal(secrets)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(encoded), testAnthropicSecret) {
		t.Fatalf("json output leaks the anthropic secret: %s", encoded)
	}
}

// TestManagedSecretConfigLoadsProviderFiles proves managed startup loads and
// scopes the four provider files through LoadConfig.
func TestManagedSecretConfigLoadsProviderFiles(t *testing.T) {
	fakeClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("MULTICA_CLAUDE_PATH", fakeClaude)
	t.Setenv("MULTICA_DAEMON_ID", "")
	t.Setenv("MULTICA_LAUNCHED_BY", "")
	paths, _ := stageManagedProviderSecrets(t)

	cfg, err := LoadConfig(Overrides{
		Managed:                    true,
		ManagedEnrollmentTokenFile: writeManagedTokenFile(t, testManagedEnrollmentToken),
		Foreground:                 true,
		ServerURL:                  "http://localhost:0",
		WorkspacesRoot:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig(managed) = %v", err)
	}
	if got := cfg.Managed.ProviderSecrets.AnthropicAPIKey.Value(); got != testAnthropicSecret {
		t.Errorf("loaded anthropic value = %q, want the staged secret", got)
	}
	if cfg.Managed.ProviderSecrets.ArkAPIKeyFile != paths.ArkAPIKey {
		t.Errorf("loaded ark file = %q, want %q", cfg.Managed.ProviderSecrets.ArkAPIKeyFile, paths.ArkAPIKey)
	}
}

// TestManagedSecretConfigReportsAnthropicFailure proves managed startup fails
// without the credential the Claude agent itself needs, without echoing the
// path or the value.
func TestManagedSecretConfigReportsAnthropicFailure(t *testing.T) {
	fakeClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("MULTICA_CLAUDE_PATH", fakeClaude)
	t.Setenv("MULTICA_DAEMON_ID", "")
	t.Setenv("MULTICA_LAUNCHED_BY", "")

	paths := managedSecretTestPaths(t)
	missing := filepath.Join(filepath.Dir(paths.AnthropicAPIKey), "absent-anthropic-api-key")
	writeManagedSecretFile(t, paths.ArkAPIKey, testArkSecret, 0o400)
	writeManagedSecretFile(t, paths.VolcASRAPIKey, testVolcASRSecret, 0o400)
	setManagedSecretPathEnv(t, managedSecretPaths{
		AnthropicAPIKey: missing,
		ArkAPIKey:       paths.ArkAPIKey,
		VolcASRAPIKey:   paths.VolcASRAPIKey,
	})

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
	if strings.Contains(err.Error(), missing) {
		t.Fatalf("error leaks the missing provider path: %v", err)
	}
	if strings.Contains(err.Error(), testAnthropicSecret) {
		t.Fatalf("error leaks a provider secret: %v", err)
	}
}

// TestManagedSecretConfigAllowsMissingOptionalProviders proves a single-route
// deployment starts when only the Claude credential is staged: the absent
// provider files do not fail managed startup.
func TestManagedSecretConfigAllowsMissingOptionalProviders(t *testing.T) {
	fakeClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("MULTICA_CLAUDE_PATH", fakeClaude)
	t.Setenv("MULTICA_DAEMON_ID", "")
	t.Setenv("MULTICA_LAUNCHED_BY", "")

	paths := managedSecretTestPaths(t)
	writeManagedSecretFile(t, paths.AnthropicAPIKey, testAnthropicSecret, 0o400)
	dir := filepath.Dir(paths.ArkAPIKey)
	setManagedSecretPathEnv(t, managedSecretPaths{
		AnthropicAPIKey: paths.AnthropicAPIKey,
		ArkAPIKey:       filepath.Join(dir, "absent-ark-api-key"),
		VolcASRAPIKey:   filepath.Join(dir, "absent-volc-asr-api-key"),
	})

	cfg, err := LoadConfig(Overrides{
		Managed:                    true,
		ManagedEnrollmentTokenFile: writeManagedTokenFile(t, testManagedEnrollmentToken),
		Foreground:                 true,
		ServerURL:                  "http://localhost:0",
		WorkspacesRoot:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig(managed) with optional provider files absent: %v", err)
	}
	if got := cfg.Managed.ProviderSecrets.AnthropicAPIKey.Value(); got != testAnthropicSecret {
		t.Errorf("loaded anthropic value = %q, want the staged secret", got)
	}
}
