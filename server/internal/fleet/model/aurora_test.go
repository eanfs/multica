package model

import (
	"errors"
	"strings"
	"testing"
)

// auroraDigest is one well-formed immutable image reference.
const auroraDigest = "@sha256:" + "a123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const auroraEgressImage = "ghcr.io/eanfs/multica-aurora-egress" + auroraDigest

const auroraConfig = `{"namespace":"local","fleet_id":"fleet-1","image":"trusted/image:dev","api_url":"http://127.0.0.1:8080","specs":{"small":{}},"aurora":{"server_url":"http://api.internal:8080","proxy_image":"ghcr.io/eanfs/multica-aurora-egress@sha256:a123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","seccomp_profile":"/etc/multica/aurora/seccomp.json","apparmor_profile":"multica-aurora-sandbox","egress_hosts":["api.example.com:443"],"anthropic_base_url":"https://ark.example.com","anthropic_model":"ark-model","provider_secret_files":{"anthropic-api-key":"/etc/multica/aurora/anthropic-api-key"},"readonly_rootfs":true,"uplink_network":"aurora-egress-uplink"}}`

func TestLoadConfigAuroraProfile(t *testing.T) {
	cfg, err := loadTestConfig(t, auroraConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Aurora == nil || cfg.Aurora.ServerURL != "http://api.internal:8080" || cfg.Aurora.ProxyImage != auroraEgressImage ||
		cfg.Aurora.SeccompProfile != "/etc/multica/aurora/seccomp.json" || cfg.Aurora.AppArmorProfile != "multica-aurora-sandbox" ||
		!cfg.Aurora.ReadonlyRootfs || cfg.Aurora.UplinkNetwork != "aurora-egress-uplink" || len(cfg.Aurora.EgressHosts) != 1 {
		t.Fatalf("aurora profile lost: %+v", cfg.Aurora)
	}
	mounts := cfg.Aurora.ProviderSecretMounts()
	if len(mounts) != 1 || mounts[0].Target != AuroraAnthropicAPIKeyTarget || mounts[0].Source != "/etc/multica/aurora/anthropic-api-key" {
		t.Fatalf("provider secret mounts = %+v", mounts)
	}
	// A configuration without the profile keeps the default Claude-only node.
	plain, err := loadTestConfig(t, validConfig)
	if err != nil || plain.Aurora != nil {
		t.Fatalf("default profile = %+v err=%v", plain.Aurora, err)
	}
}

func TestLoadConfigRejectsUnsafeAuroraProfile(t *testing.T) {
	cases := map[string]string{
		"missing server url":      strings.Replace(auroraConfig, `"server_url":"http://api.internal:8080"`, "", 1),
		"credentials in url":      strings.Replace(auroraConfig, "http://api.internal:8080", "http://user:pass@api.internal:8080", 1),
		"path in url":             strings.Replace(auroraConfig, "http://api.internal:8080", "http://api.internal:8080/api", 1),
		"tag-only proxy":          strings.Replace(auroraConfig, auroraEgressImage, "ghcr.io/eanfs/multica-aurora-egress:latest", 1),
		"uppercase proxy digest":  strings.Replace(auroraConfig, "a123456789abcdef", "A123456789ABCDEF", 1),
		"relative seccomp":        strings.Replace(auroraConfig, "/etc/multica/aurora/seccomp.json", "seccomp.json", 1),
		"dirty seccomp":           strings.Replace(auroraConfig, "/etc/multica/aurora/seccomp.json", "/etc/multica/aurora/../seccomp.json", 1),
		"empty apparmor":          strings.Replace(auroraConfig, "multica-aurora-sandbox", "", 1),
		"wildcard egress":         strings.Replace(auroraConfig, "api.example.com:443", "*.example.com:443", 1),
		"non-443 egress":          strings.Replace(auroraConfig, "api.example.com:443", "api.example.com:8443", 1),
		"egress without port":     strings.Replace(auroraConfig, "api.example.com:443", "api.example.com", 1),
		"http anthropic base":     strings.Replace(auroraConfig, "https://ark.example.com", "http://ark.example.com", 1),
		"anthropic path":          strings.Replace(auroraConfig, "https://ark.example.com", "https://ark.example.com/v1", 1),
		"unknown secret target":   strings.Replace(auroraConfig, "anthropic-api-key", "evil-key", 1),
		"relative secret source":  strings.Replace(auroraConfig, "/etc/multica/aurora/anthropic-api-key", "anthropic-api-key", 1),
		"readonly false":          strings.Replace(auroraConfig, `"readonly_rootfs":true`, `"readonly_rootfs":false`, 1),
		"missing readonly":        strings.Replace(auroraConfig, `"readonly_rootfs":true,`, "", 1),
		"empty uplink":            strings.Replace(auroraConfig, "aurora-egress-uplink", "", 1),
		"unsafe uplink":           strings.Replace(auroraConfig, "aurora-egress-uplink", "uplink/evil", 1),
		"unknown nested key":      strings.Replace(auroraConfig, `"aurora":{"server_url"`, `"aurora":{"image":"evil","server_url"`, 1),
		"nested null":             strings.Replace(auroraConfig, `"readonly_rootfs":true`, `"readonly_rootfs":null`, 1),
		"server url type":         strings.Replace(auroraConfig, `"server_url":"http://api.internal:8080"`, `"server_url":7`, 1),
		"provider files as array": strings.Replace(auroraConfig, `"provider_secret_files":{"anthropic-api-key":"/etc/multica/aurora/anthropic-api-key"}`, `"provider_secret_files":[]`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadTestConfig(t, raw)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("LoadConfig error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// TestAuroraClaudePathContract pins the one agent executable path the provider
// supplies at container start. The neutral image bakes no MULTICA_CLAUDE_PATH,
// so this exact pairing is the whole runtime contract the Fleet must satisfy.
func TestAuroraClaudePathContract(t *testing.T) {
	if AuroraClaudePathEnv != "MULTICA_CLAUDE_PATH" {
		t.Fatalf("AuroraClaudePathEnv = %q", AuroraClaudePathEnv)
	}
	if AuroraClaudePath != "/opt/aurora/runtime/node_modules/.bin/claude" {
		t.Fatalf("AuroraClaudePath = %q", AuroraClaudePath)
	}
}

func TestValidEnrollmentToken(t *testing.T) {
	valid := "mse_" + strings.Repeat("a", 40)
	if !ValidEnrollmentToken(valid) {
		t.Fatal("server-issued enrollment token must be accepted")
	}
	for name, token := range map[string]string{
		"empty":        "",
		"wrong prefix": "msx_" + strings.Repeat("a", 40),
		"short":        "mse_" + strings.Repeat("a", 39),
		"long":         "mse_" + strings.Repeat("a", 41),
		"uppercase":    "mse_" + strings.Repeat("A", 40),
		"non hex":      "mse_" + strings.Repeat("z", 40),
		"node token":   "mcn_" + strings.Repeat("a", 43),
	} {
		t.Run(name, func(t *testing.T) {
			if ValidEnrollmentToken(token) {
				t.Fatalf("token %q must be rejected", token)
			}
		})
	}
}
