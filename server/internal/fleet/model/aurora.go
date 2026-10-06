package model

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// The managed Aurora sandbox runtime reads exactly one single-use enrollment
// secret from a fixed read-only path. The Fleet writes that file into the node
// secrets volume and mounts the volume read-only at AuroraEnrollmentDir, so the
// path is part of the fixed layout contract, never caller input.
const (
	AuroraEnrollmentDir     = "/secrets"
	AuroraEnrollmentFile    = AuroraEnrollmentDir + "/aurora-enrollment"
	AuroraManagedEnv        = "MULTICA_MANAGED"
	AuroraServerURLEnv      = "MULTICA_SERVER_URL"
	AuroraEnrollmentFileEnv = "MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE"

	// AuroraHTTPProxyEnv, AuroraHTTPSProxyEnv and AuroraNoProxyEnv are the only
	// proxy variables the managed sandbox may inherit. The node never joins the
	// uplink network; the proxy is its only egress.
	AuroraHTTPProxyEnv  = "HTTP_PROXY"
	AuroraHTTPSProxyEnv = "HTTPS_PROXY"
	AuroraNoProxyEnv    = "NO_PROXY"

	// AuroraAnthropicBaseURLEnv and AuroraAnthropicModelEnv forward the optional
	// operator-configured managed Claude endpoint. They are never credentials.
	AuroraAnthropicBaseURLEnv = "ANTHROPIC_BASE_URL"
	AuroraAnthropicModelEnv   = "ANTHROPIC_MODEL"

	// AuroraEgressAlias is the Docker network alias the egress sidecar takes on
	// the workspace-internal network, so the sandbox can reach it by name.
	AuroraEgressAlias = "egress"
	// AuroraEgressProxyEndpoint is the only proxy endpoint the sandbox may use.
	AuroraEgressProxyEndpoint = "http://" + AuroraEgressAlias + ":3128"
	// AuroraNoProxyValue keeps the proxy itself and loopback out of the proxy.
	AuroraNoProxyValue = "egress,127.0.0.1,localhost"

	// Fixed provider credential destinations inside the sandbox. The managed
	// daemon and the MCP broker read these exact paths; the Fleet mounts operator
	// files there read-only and never passes a credential value.
	AuroraAnthropicAPIKeyTarget = "/run/secrets/anthropic-api-key"
	AuroraArkAPIKeyTarget       = "/run/secrets/ark-api-key"
	AuroraOpenAIAPIKeyTarget    = "/run/secrets/openai-api-key"
	AuroraVolcASRAPIKeyTarget   = "/run/secrets/volc-asr-api-key"

	// AuroraWorkspaceMount, AuroraTmpMount and AuroraRunMount are the fixed
	// writable tmpfs surfaces of the Aurora node. Every one is nosuid,nodev,noexec
	// and owned by the sandbox user; mount denial itself belongs to seccomp.
	AuroraWorkspaceMount = "/workspace"
	AuroraTmpMount       = "/tmp"
	AuroraRunMount       = "/run"
)

// enrollmentTokenPattern matches the server-issued managed enrollment secret:
// "mse_" plus 40 lowercase hex characters. Validation is local so junk secrets
// never reach the managed daemon or the enrollment endpoint.
var enrollmentTokenPattern = regexp.MustCompile("^mse_[0-9a-f]{40}$")

// providerSecretOrder fixes the mount order so one configuration always yields
// one deterministic container specification.
var providerSecretOrder = []string{"anthropic-api-key", "ark-api-key", "openai-api-key", "volc-asr-api-key"}

// ProviderSecretTargets maps each accepted ProviderSecretFiles key to its fixed
// in-container destination. Keys outside this map are rejected by Validate, so a
// control plane can never choose a destination.
var ProviderSecretTargets = map[string]string{
	"anthropic-api-key": AuroraAnthropicAPIKeyTarget,
	"ark-api-key":       AuroraArkAPIKeyTarget,
	"openai-api-key":    AuroraOpenAIAPIKeyTarget,
	"volc-asr-api-key":  AuroraVolcASRAPIKeyTarget,
}

// ProviderSecretMount is one operator-staged read-only credential source and the
// fixed destination it appears at inside the sandbox.
type ProviderSecretMount struct {
	Key    string
	Source string
	Target string
}

// ProviderSecretMounts returns the configured provider credential mounts in
// fixed target order. Empty entries mount nothing.
func (a AuroraConfig) ProviderSecretMounts() []ProviderSecretMount {
	mounts := make([]ProviderSecretMount, 0, len(a.ProviderSecretFiles))
	for _, key := range providerSecretOrder {
		source := strings.TrimSpace(a.ProviderSecretFiles[key])
		if source == "" {
			continue
		}
		mounts = append(mounts, ProviderSecretMount{Key: key, Source: source, Target: ProviderSecretTargets[key]})
	}
	return mounts
}

// AuroraConfig selects the managed-sandbox execution profile for one Fleet
// deployment. It is administrator-owned public configuration: the sandbox image
// is Config.Image and model credentials belong to the sandbox's own provider
// secret mounts, never here. When nil the deployment runs the default
// Claude-only node profile.
type AuroraConfig struct {
	// ServerURL is the container-reachable Multica API origin the managed
	// daemon enrolls against and calls back to. It is not a client-supplied
	// image, path or credential.
	ServerURL string `json:"server_url"`
	// ProxyImage is the digest-pinned egress sidecar image. It is the only
	// network path off the workspace-internal network.
	ProxyImage string `json:"proxy_image"`
	// SeccompProfile is the absolute host path of the deployed seccomp profile.
	SeccompProfile string `json:"seccomp_profile"`
	// AppArmorProfile is the loaded AppArmor profile name referenced by the
	// container. Loading the profile is an operator step.
	AppArmorProfile string `json:"apparmor_profile"`
	// EgressHosts is the explicit exact host:443 allowlist the sidecar accepts
	// in addition to the provider hosts compiled into the proxy image.
	EgressHosts []string `json:"egress_hosts"`
	// AnthropicBaseURL and AnthropicModel forward the optional managed Claude
	// endpoint override. Empty preserves the provider default.
	AnthropicBaseURL string `json:"anthropic_base_url"`
	AnthropicModel   string `json:"anthropic_model"`
	// ProviderSecretFiles maps each fixed target name to an absolute host file
	// mounted read-only at /run/secrets/<name>. Empty values mount nothing.
	ProviderSecretFiles map[string]string `json:"provider_secret_files"`
	// ReadonlyRootfs must be explicitly true: the managed profile never opts out
	// of the read-only root filesystem.
	ReadonlyRootfs bool `json:"readonly_rootfs"`
	// UplinkNetwork is the operator-provided Docker network only the egress
	// sidecar joins. The sandbox never joins it.
	UplinkNetwork string `json:"uplink_network"`
}

// Validate rejects an Aurora profile that could not safely enroll or isolate.
// Every field is checked locally before any Docker resource is built, so a
// malformed profile fails closed with no partial provisioning.
func (a AuroraConfig) Validate() error {
	if !ValidOrigin(a.ServerURL) {
		return fmt.Errorf("%w: aurora server_url must be an http(s) origin", ErrInvalidRequest)
	}
	if !digestPinnedImagePattern.MatchString(a.ProxyImage) {
		return fmt.Errorf("%w: aurora proxy_image must be pinned as <name>@sha256:<64 lowercase hex>", ErrInvalidRequest)
	}
	if !filepath.IsAbs(a.SeccompProfile) || filepath.Clean(a.SeccompProfile) != a.SeccompProfile {
		return fmt.Errorf("%w: aurora seccomp_profile must be a clean absolute host path", ErrInvalidRequest)
	}
	if !profileNamePattern.MatchString(a.AppArmorProfile) {
		return fmt.Errorf("%w: aurora apparmor_profile must be a loaded profile name", ErrInvalidRequest)
	}
	for _, host := range a.EgressHosts {
		if !validEgressHost(host) {
			return fmt.Errorf("%w: aurora egress_hosts must be exact host:443 entries", ErrInvalidRequest)
		}
	}
	if a.AnthropicBaseURL != "" && !ValidAnthropicBaseURL(a.AnthropicBaseURL) {
		return fmt.Errorf("%w: aurora anthropic_base_url must be a single https host without credentials, query or fragment", ErrInvalidRequest)
	}
	if a.AnthropicModel != "" && !validModelName(a.AnthropicModel) {
		return fmt.Errorf("%w: aurora anthropic_model must be a non-empty model name", ErrInvalidRequest)
	}
	for key, source := range a.ProviderSecretFiles {
		if _, ok := ProviderSecretTargets[key]; !ok {
			return fmt.Errorf("%w: aurora provider_secret_files target name is not fixed", ErrInvalidRequest)
		}
		if source == "" {
			continue
		}
		if strings.TrimSpace(source) != source || !filepath.IsAbs(source) || filepath.Clean(source) != source || !cleanPathPattern.MatchString(source) {
			return fmt.Errorf("%w: aurora provider_secret_files source must be a clean absolute host file path", ErrInvalidRequest)
		}
	}
	if !a.ReadonlyRootfs {
		return fmt.Errorf("%w: aurora readonly_rootfs must be explicitly true", ErrInvalidRequest)
	}
	if !networkNamePattern.MatchString(a.UplinkNetwork) {
		return fmt.Errorf("%w: aurora uplink_network must be a Docker network name", ErrInvalidRequest)
	}
	return nil
}

var (
	// digestPinnedImagePattern matches an immutable OCI reference pinned with a
	// lowercase sha256 digest. Tag-only references are rejected.
	digestPinnedImagePattern = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$")
	// profileNamePattern is a conservative AppArmor profile name.
	profileNamePattern = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$")
	// networkNamePattern is a conservative Docker network name.
	networkNamePattern = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$")
	// cleanPathPattern rejects control characters and whitespace in mount paths.
	cleanPathPattern = regexp.MustCompile("^/[!-~]+$")
)

// validEgressHost accepts only an exact host:443 entry with no wildcard.
func validEgressHost(raw string) bool {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" || host != raw || strings.ContainsAny(host, "*? ") {
		return false
	}
	name, port, err := net.SplitHostPort(host)
	return err == nil && name != "" && port == "443" && !strings.Contains(name, "/")
}

// ValidAnthropicBaseURL requires a single https host with no credentials, path,
// query or fragment. It mirrors the managed daemon's own startup validation.
func ValidAnthropicBaseURL(raw string) bool {
	if strings.TrimSpace(raw) != raw || raw == "" || strings.Contains(raw, "#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() != "" {
		return false
	}
	return u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == ""
}

// validModelName requires a single opaque model token with no whitespace.
func validModelName(raw string) bool {
	if raw == "" || strings.TrimSpace(raw) != raw || len(raw) > 128 {
		return false
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// ValidEnrollmentToken reports whether token matches the server-issued managed
// enrollment format. The Fleet never derives identity from the secret.
func ValidEnrollmentToken(token string) bool {
	return enrollmentTokenPattern.MatchString(token)
}

// ValidOrigin accepts only a bare http(s) origin, so a configured server URL
// cannot smuggle credentials, a path, a query or a fragment into the managed
// daemon environment.
func ValidOrigin(raw string) bool {
	if strings.TrimSpace(raw) != raw || raw == "" || strings.Contains(raw, "#") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Path == "" && u.RawPath == ""
}
