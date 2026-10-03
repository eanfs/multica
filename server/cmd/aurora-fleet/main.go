// Command aurora-fleet is the Aurora workspace sandbox fleet controller.
//
// It exposes the authenticated internal workspace-node API the main server
// calls to ensure, inspect, and delete per-workspace sandbox nodes.
// Configuration is environment-driven:
//
//	AURORA_FLEET_ADDR                  listen address (default ":8081")
//	AURORA_FLEET_BACKEND               node backend: "docker" or "memory" (default "docker")
//	AURORA_FLEET_CONTROL_TOKEN_FILE    file holding the fleet control bearer token (required)
//	AURORA_FLEET_SECRET_ROOT           root directory for staged enrollment secrets (required)
//
// The docker backend additionally requires the immutable policy variables:
//
//	AURORA_SANDBOX_IMAGE               digest-pinned sandbox OCI reference (required)
//	AURORA_PROXY_IMAGE                 digest-pinned egress sidecar OCI reference (required)
//	AURORA_SECCOMP_PROFILE             absolute path to the deployed seccomp profile (required)
//	AURORA_EGRESS_SERVER_ORIGIN        exact Multica server origin sandboxes call back to (required)
//	AURORA_EGRESS_ALLOWED_HOSTS        extra exact host:443 egress allowlist entries (optional)
//
// The docker backend also forwards the optional Anthropic-compatible endpoint
// overrides the managed daemon reads from its own process environment
// (server/internal/daemon/managed_secrets.go). They are not secrets and are
// only passed through when set:
//
//	ANTHROPIC_BASE_URL   managed Claude endpoint base URL (optional, https)
//	ANTHROPIC_MODEL      managed Claude model name (optional)
//
// The policy also mounts the operator-staged provider credential files. Each
// variable is optional and names one absolute host file under
// AURORA_FLEET_SECRET_ROOT; the file is mounted read-only at its fixed sandbox
// destination and the value is never passed in the container environment:
//
//	ANTHROPIC_API_KEY_FILE   Claude agent credential  -> /run/secrets/anthropic-api-key
//	ARK_API_KEY_FILE         Volcengine Ark credential -> /run/secrets/ark-api-key
//	OPENAI_API_KEY_FILE      OpenAI credential        -> /run/secrets/openai-api-key
//	VOLC_ASR_API_KEY_FILE    Volcengine ASR credential -> /run/secrets/volc-asr-api-key
//
// An unset variable mounts nothing rather than substituting a default path; a
// set-but-missing or unsafe file fails the ensure instead of being invented.
//
// The control token file must be a regular file with mode 0400 or 0600
// holding at least 32 random bytes as base64url or hex. The token is never
// accepted from the environment and never logged.
//
// A non-loopback listen address is a startup error: the fleet API is an
// internal control surface, and remote binds require the TLS configuration
// that arrives with the isolation hardening.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/aurorafleet"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("aurora-fleet failed", "error", err)
		os.Exit(1)
	}
	slog.Info("aurora-fleet stopped")
}

func run() error {
	addr := envOr("AURORA_FLEET_ADDR", ":8081")
	if host, _, err := net.SplitHostPort(addr); err == nil && host != "" && !isLoopbackHostMain(host) {
		return fmt.Errorf("listen address %s is not loopback: the fleet API requires a TLS-terminated remote bind, which is not supported yet", addr)
	}

	tokenFile := strings.TrimSpace(os.Getenv("AURORA_FLEET_CONTROL_TOKEN_FILE"))
	if tokenFile == "" {
		return errors.New("AURORA_FLEET_CONTROL_TOKEN_FILE is required")
	}
	secretRoot := strings.TrimSpace(os.Getenv("AURORA_FLEET_SECRET_ROOT"))
	if secretRoot == "" {
		return errors.New("AURORA_FLEET_SECRET_ROOT is required")
	}
	auth, err := aurorafleet.LoadControlAuth(tokenFile)
	if err != nil {
		return fmt.Errorf("load fleet control token: %w", err)
	}

	backend, err := backendFromEnv()
	if err != nil {
		return err
	}
	ctrl := aurorafleet.NewController(aurorafleet.Config{
		Backend:    backend,
		Auth:       auth,
		SecretRoot: secretRoot,
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           ctrl.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("aurora-fleet listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("aurora-fleet server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("aurora-fleet shutdown failed", "error", err)
	}
	return nil
}

// backendFromEnv selects the node backend. "memory" is the zero-dependency
// in-process backend for development and tests; "docker" (the default) shells
// out to the docker CLI using the immutable policy assembled from the
// deployment's environment. An incomplete docker policy fails the process at
// startup rather than provisioning an under-configured container later.
func backendFromEnv() (aurorafleet.Backend, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AURORA_FLEET_BACKEND")), "memory") {
		return aurorafleet.NewMemoryBackend(), nil
	}
	policy, err := dockerPolicyFromEnv()
	if err != nil {
		return nil, err
	}
	return aurorafleet.NewDockerBackendWithPolicy(policy), nil
}

// dockerPolicyFromEnv assembles and validates the immutable Docker policy from
// the fleet process environment. It is a separate function so the provider
// secret wiring is testable without a Docker daemon.
func dockerPolicyFromEnv() (aurorafleet.Policy, error) {
	policy := aurorafleet.Policy{
		SandboxImage:     strings.TrimSpace(os.Getenv("AURORA_SANDBOX_IMAGE")),
		ProxyImage:       strings.TrimSpace(os.Getenv("AURORA_PROXY_IMAGE")),
		SeccompPath:      strings.TrimSpace(os.Getenv("AURORA_SECCOMP_PROFILE")),
		ServerOrigin:     strings.TrimSpace(os.Getenv("AURORA_EGRESS_SERVER_ORIGIN")),
		SecretRoot:       strings.TrimSpace(os.Getenv("AURORA_FLEET_SECRET_ROOT")),
		ExtraEgressHosts: splitHosts(os.Getenv("AURORA_EGRESS_ALLOWED_HOSTS")),
		// The managed Claude endpoint overrides are operator process
		// configuration. The daemon validates them at startup; the fleet only
		// forwards a supplied pair so a real endpoint is reachable through a
		// fleet instead of the per-agent custom_env path.
		AnthropicBaseURL: strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
		AnthropicModel:   strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL")),
		// The control API can never choose a provider source or destination;
		// these are operator-staged host paths read once from the environment.
		ProviderSecretFiles: providerSecretFilesFromEnv(),
	}
	if err := policy.Validate(); err != nil {
		return aurorafleet.Policy{}, fmt.Errorf("docker fleet policy: %w", err)
	}
	return policy, nil
}

// providerSecretFilesFromEnv reads the documented *_API_KEY_FILE variables. Each
// names an absolute host file under AURORA_FLEET_SECRET_ROOT. An unset variable
// stays empty, so providerSecretMountArgs mounts nothing for it and never
// substitutes a default; a set-but-missing or unsafe file is rejected when the
// sandbox argv is built.
func providerSecretFilesFromEnv() aurorafleet.ProviderSecretFiles {
	return aurorafleet.ProviderSecretFiles{
		AnthropicAPIKey: strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY_FILE")),
		ArkAPIKey:       strings.TrimSpace(os.Getenv("ARK_API_KEY_FILE")),
		OpenAIAPIKey:    strings.TrimSpace(os.Getenv("OPENAI_API_KEY_FILE")),
		VolcASRAPIKey:   strings.TrimSpace(os.Getenv("VOLC_ASR_API_KEY_FILE")),
	}
}

// splitHosts parses a comma-separated egress host list, dropping empty entries.
func splitHosts(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func isLoopbackHostMain(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
