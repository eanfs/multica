package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/aurorafleet"
)

// testDigest is a valid digest-pinned image reference for the policy tests.
var testDigest = "sha256:" + strings.Repeat("a", 64)

// TestDockerPolicyFromEnvMountsProviderSecrets is the provider-credential
// wiring regression: the documented *_API_KEY_FILE variables must reach the
// hardened docker argv as read-only mounts at their fixed sandbox destinations,
// and an unset variable must mount nothing.
func TestDockerPolicyFromEnvMountsProviderSecrets(t *testing.T) {
	root := t.TempDir()
	arkFile := filepath.Join(root, "ark-api-key")
	if err := os.WriteFile(arkFile, []byte("mount-proof"), 0o400); err != nil {
		t.Fatalf("stage ark credential: %v", err)
	}
	enrollment := filepath.Join(root, "node", "enrollment")
	if err := os.MkdirAll(filepath.Dir(enrollment), 0o700); err != nil {
		t.Fatalf("mkdir enrollment dir: %v", err)
	}
	if err := os.WriteFile(enrollment, []byte("mse_test"), 0o400); err != nil {
		t.Fatalf("stage enrollment: %v", err)
	}

	t.Setenv("AURORA_FLEET_SECRET_ROOT", root)
	t.Setenv("AURORA_SANDBOX_IMAGE", testDigest)
	t.Setenv("AURORA_PROXY_IMAGE", testDigest)
	t.Setenv("AURORA_SECCOMP_PROFILE", filepath.Join(root, "seccomp.json"))
	t.Setenv("AURORA_EGRESS_SERVER_ORIGIN", "http://127.0.0.1:9")
	t.Setenv("ARK_API_KEY_FILE", arkFile)
	t.Setenv("ANTHROPIC_API_KEY_FILE", "")
	t.Setenv("OPENAI_API_KEY_FILE", "")
	t.Setenv("VOLC_ASR_API_KEY_FILE", "")

	policy, err := dockerPolicyFromEnv()
	if err != nil {
		t.Fatalf("dockerPolicyFromEnv: %v", err)
	}

	spec := aurorafleet.WorkspaceNodeSpec{
		NodeID:         "01933e60-0000-7d4e-9f01-2a3b4c5d6e01",
		WorkspaceID:    "01933e60-0000-7d4e-9f01-2a3b4c5d6e02",
		RuntimeID:      "01933e60-0000-7d4e-9f01-2a3b4c5d6e03",
		DaemonID:       "01933e60-0000-7d4e-9f01-2a3b4c5d6e04",
		EnrollmentFile: enrollment,
	}
	args, err := policy.SandboxArgs(spec)
	if err != nil {
		t.Fatalf("SandboxArgs: %v", err)
	}
	want := []string{"--mount", "type=bind,src=" + arkFile + ",dst=/run/secrets/ark-api-key,readonly"}
	if !containsSubslice(args, want) {
		t.Fatalf("sandbox args missing the ark provider mount %v: %v", want, args)
	}
	for _, arg := range args {
		if strings.Contains(arg, "/run/secrets/anthropic-api-key") ||
			strings.Contains(arg, "/run/secrets/openai-api-key") ||
			strings.Contains(arg, "/run/secrets/volc-asr-api-key") {
			t.Fatalf("unset provider variable produced a mount: %q", arg)
		}
	}
}

func containsSubslice(args, want []string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestProviderSecretFilesFromEnv pins the variable-to-field mapping and that a
// missing variable stays empty rather than falling back to a default path.
func TestProviderSecretFilesFromEnv(t *testing.T) {
	root := t.TempDir()
	stage := func(name string) string {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("mount-proof"), 0o400); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
		return p
	}
	want := aurorafleet.ProviderSecretFiles{
		AnthropicAPIKey: stage("anthropic-api-key"),
		ArkAPIKey:       stage("ark-api-key"),
		OpenAIAPIKey:    stage("openai-api-key"),
		VolcASRAPIKey:   stage("volc-asr-api-key"),
	}
	t.Setenv("ANTHROPIC_API_KEY_FILE", want.AnthropicAPIKey)
	t.Setenv("ARK_API_KEY_FILE", want.ArkAPIKey)
	t.Setenv("OPENAI_API_KEY_FILE", want.OpenAIAPIKey)
	t.Setenv("VOLC_ASR_API_KEY_FILE", want.VolcASRAPIKey)

	if got := providerSecretFilesFromEnv(); got != want {
		t.Fatalf("providerSecretFilesFromEnv() = %+v, want %+v", got, want)
	}
}

func TestProviderSecretFilesFromEnvAreEmptyWhenUnset(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY_FILE", "")
	t.Setenv("ARK_API_KEY_FILE", "")
	t.Setenv("OPENAI_API_KEY_FILE", "")
	t.Setenv("VOLC_ASR_API_KEY_FILE", "")
	if got := providerSecretFilesFromEnv(); got != (aurorafleet.ProviderSecretFiles{}) {
		t.Fatalf("providerSecretFilesFromEnv() = %+v, want the zero value", got)
	}
}

// TestDockerPolicyFromEnvReadsManagedEndpointOverrides pins the PR #178
// passthrough: the documented ANTHROPIC_BASE_URL/ANTHROPIC_MODEL variables must
// reach the fleet policy so SandboxArgs can forward them. An unset pair stays
// empty rather than inventing a default endpoint.
func TestDockerPolicyFromEnvReadsManagedEndpointOverrides(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AURORA_FLEET_SECRET_ROOT", root)
	t.Setenv("AURORA_SANDBOX_IMAGE", testDigest)
	t.Setenv("AURORA_PROXY_IMAGE", testDigest)
	t.Setenv("AURORA_SECCOMP_PROFILE", filepath.Join(root, "seccomp.json"))
	t.Setenv("AURORA_EGRESS_SERVER_ORIGIN", "http://127.0.0.1:9")
	t.Setenv("ANTHROPIC_BASE_URL", "https://ark.cn-beijing.volces.com/api/plan")
	t.Setenv("ANTHROPIC_MODEL", "claude-sonnet-4-5")

	policy, err := dockerPolicyFromEnv()
	if err != nil {
		t.Fatalf("dockerPolicyFromEnv: %v", err)
	}
	if policy.AnthropicBaseURL != "https://ark.cn-beijing.volces.com/api/plan" {
		t.Fatalf("AnthropicBaseURL = %q", policy.AnthropicBaseURL)
	}
	if policy.AnthropicModel != "claude-sonnet-4-5" {
		t.Fatalf("AnthropicModel = %q", policy.AnthropicModel)
	}

	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	unset, err := dockerPolicyFromEnv()
	if err != nil {
		t.Fatalf("dockerPolicyFromEnv(unset): %v", err)
	}
	if unset.AnthropicBaseURL != "" || unset.AnthropicModel != "" {
		t.Fatalf("unset endpoint overrides = %q / %q, want empty", unset.AnthropicBaseURL, unset.AnthropicModel)
	}
}
