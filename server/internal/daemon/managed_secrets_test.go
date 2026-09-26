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
	testOpenAISecret    = "sk-openai-managed-test-value"
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
		OpenAIAPIKey:    filepath.Join(dir, "openai-api-key"),
		VolcASRAPIKey:   filepath.Join(dir, "volc-asr-api-key"),
	}
}

// setManagedSecretPathEnv points the documented *_API_KEY_FILE overrides at
// the supplied paths.
func setManagedSecretPathEnv(t *testing.T, paths managedSecretPaths) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY_FILE", paths.AnthropicAPIKey)
	t.Setenv("ARK_API_KEY_FILE", paths.ArkAPIKey)
	t.Setenv("OPENAI_API_KEY_FILE", paths.OpenAIAPIKey)
	t.Setenv("VOLC_ASR_API_KEY_FILE", paths.VolcASRAPIKey)
}

// stageManagedProviderSecrets writes the four valid provider secret files,
// points the env overrides at them, and loads them through the production
// loader.
func stageManagedProviderSecrets(t *testing.T) (managedSecretPaths, managedProviderSecrets) {
	t.Helper()
	paths := managedSecretTestPaths(t)
	writeManagedSecretFile(t, paths.AnthropicAPIKey, testAnthropicSecret+"\n", 0o400)
	writeManagedSecretFile(t, paths.ArkAPIKey, testArkSecret+"\n", 0o400)
	writeManagedSecretFile(t, paths.OpenAIAPIKey, testOpenAISecret+"\n", 0o400)
	writeManagedSecretFile(t, paths.VolcASRAPIKey, testVolcASRSecret+"\n", 0o400)
	setManagedSecretPathEnv(t, paths)
	secrets, err := loadManagedProviderSecrets(paths)
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

// TestManagedSecretLoaderRequiresEveryProvider proves a missing provider file
// fails with a provider-specific error that names no path and no value.
func TestManagedSecretLoaderRequiresEveryProvider(t *testing.T) {
	paths := managedSecretTestPaths(t)
	writeManagedSecretFile(t, paths.AnthropicAPIKey, testAnthropicSecret, 0o400)
	writeManagedSecretFile(t, paths.ArkAPIKey, testArkSecret, 0o400)
	writeManagedSecretFile(t, paths.VolcASRAPIKey, testVolcASRSecret, 0o400)
	// The OpenAI file is deliberately absent.

	_, err := loadManagedProviderSecrets(paths)
	if err == nil {
		t.Fatal("loadManagedProviderSecrets accepted a missing provider file")
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Fatalf("error is not provider-specific: %v", err)
	}
	if strings.Contains(err.Error(), paths.OpenAIAPIKey) {
		t.Fatalf("error leaks the missing path: %v", err)
	}
	if strings.Contains(err.Error(), testAnthropicSecret) {
		t.Fatalf("error leaks a secret value: %v", err)
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
	if secrets.OpenAIAPIKeyFile != paths.OpenAIAPIKey {
		t.Errorf("openai file = %q, want %q", secrets.OpenAIAPIKeyFile, paths.OpenAIAPIKey)
	}
	if secrets.VolcASRAPIKeyFile != paths.VolcASRAPIKey {
		t.Errorf("asr file = %q, want %q", secrets.VolcASRAPIKeyFile, paths.VolcASRAPIKey)
	}
}

// TestManagedSecretChildEnvScoping proves the Anthropic value reaches only the
// Claude child, and the three provider file paths reach only the MCP broker.
func TestManagedSecretChildEnvScoping(t *testing.T) {
	_, secrets := stageManagedProviderSecrets(t)

	claude := secrets.claudeChildEnv()
	if len(claude) != 1 {
		t.Fatalf("claude child env = %v, want only ANTHROPIC_API_KEY", claude)
	}
	if claude["ANTHROPIC_API_KEY"] != testAnthropicSecret {
		t.Fatalf("claude child env value = %q, want the anthropic secret", claude["ANTHROPIC_API_KEY"])
	}
	for _, name := range []string{"ARK_API_KEY_FILE", "OPENAI_API_KEY_FILE", "VOLC_ASR_API_KEY_FILE"} {
		if _, ok := claude[name]; ok {
			t.Errorf("claude child env carries broker name %s", name)
		}
	}

	broker := secrets.mcpBrokerChildEnv()
	if len(broker) != 3 {
		t.Fatalf("mcp broker env = %v, want the three provider file paths", broker)
	}
	for name, want := range map[string]string{
		"ARK_API_KEY_FILE":      secrets.ArkAPIKeyFile,
		"OPENAI_API_KEY_FILE":   secrets.OpenAIAPIKeyFile,
		"VOLC_ASR_API_KEY_FILE": secrets.VolcASRAPIKeyFile,
	} {
		if broker[name] != want {
			t.Errorf("mcp broker env[%s] = %q, want %q", name, broker[name], want)
		}
	}
	if _, ok := broker["ANTHROPIC_API_KEY"]; ok {
		t.Error("mcp broker env carries the Anthropic credential name")
	}
	for _, value := range broker {
		if value == testAnthropicSecret {
			t.Error("mcp broker env carries the Anthropic credential value")
		}
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

// TestManagedSecretConfigReportsProviderSpecificFailure proves a missing
// provider file fails managed startup without echoing the path or the value.
func TestManagedSecretConfigReportsProviderSpecificFailure(t *testing.T) {
	fakeClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("MULTICA_CLAUDE_PATH", fakeClaude)
	t.Setenv("MULTICA_DAEMON_ID", "")
	t.Setenv("MULTICA_LAUNCHED_BY", "")

	paths := managedSecretTestPaths(t)
	missing := filepath.Join(filepath.Dir(paths.ArkAPIKey), "absent-ark-api-key")
	writeManagedSecretFile(t, paths.AnthropicAPIKey, testAnthropicSecret, 0o400)
	writeManagedSecretFile(t, paths.OpenAIAPIKey, testOpenAISecret, 0o400)
	writeManagedSecretFile(t, paths.VolcASRAPIKey, testVolcASRSecret, 0o400)
	setManagedSecretPathEnv(t, managedSecretPaths{
		AnthropicAPIKey: paths.AnthropicAPIKey,
		ArkAPIKey:       missing,
		OpenAIAPIKey:    paths.OpenAIAPIKey,
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
		t.Fatal("LoadConfig(managed) accepted a missing provider file")
	}
	if !strings.Contains(err.Error(), "ark") {
		t.Fatalf("error is not provider-specific: %v", err)
	}
	if strings.Contains(err.Error(), missing) {
		t.Fatalf("error leaks the missing provider path: %v", err)
	}
	if strings.Contains(err.Error(), testAnthropicSecret) {
		t.Fatalf("error leaks a provider secret: %v", err)
	}
}
