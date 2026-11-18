package fleet

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

const (
	// defaultListen keeps the control plane private on a normal host. Inside a
	// container the compose file still publishes only 127.0.0.1 on the host, so
	// the container may bind 0.0.0.0 when MULTICA_FLEET_CONTAINER=1.
	defaultListen = "127.0.0.1:8091"

	defaultMaxNodesPerOwner = 2
	containerEnv            = "MULTICA_FLEET_CONTAINER"
)

// Spec is one allowed node size. Clients may only pick from this allowlist.
type Spec struct {
	ID            string  `json:"id"`
	CPU           float64 `json:"cpu"`
	MemoryMB      int64   `json:"memory_mb"`
	PidsLimit     int64   `json:"pids_limit"`
	MaxConcurrent int     `json:"max_concurrent"`
}

// Config is the fully validated Fleet configuration for one checkout.
type Config struct {
	Listen           string
	Namespace        string
	SecretFile       string
	DockerHost       string
	NodeNetwork      string
	RuntimeImage     string
	APIURL           string
	DatabaseURL      string
	CredentialsFile  string
	MaxNodesPerOwner int
	Specs            []Spec
}

// Load reads Fleet configuration from getenv (normally os.Getenv) and fails
// closed: a missing value, a malformed value or an unreadable secret file is a
// startup error, never a silent fallback.
func Load(getenv func(string) string) (Config, error) {
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	cfg := Config{
		Listen:           get("MULTICA_FLEET_LISTEN"),
		Namespace:        get("MULTICA_FLEET_NAMESPACE"),
		SecretFile:       get("MULTICA_FLEET_SECRET_FILE"),
		DockerHost:       get("MULTICA_FLEET_DOCKER_HOST"),
		NodeNetwork:      get("MULTICA_FLEET_NODE_NETWORK"),
		RuntimeImage:     get("MULTICA_FLEET_RUNTIME_IMAGE"),
		APIURL:           get("MULTICA_FLEET_API_URL"),
		DatabaseURL:      get("DATABASE_URL"),
		CredentialsFile:  get("MULTICA_FLEET_CREDENTIALS_FILE"),
		MaxNodesPerOwner: defaultMaxNodesPerOwner,
		Specs: []Spec{{
			ID:            "standard",
			CPU:           2,
			MemoryMB:      4096,
			PidsLimit:     256,
			MaxConcurrent: 1,
		}},
	}
	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}

	// Keep the required-field check ordered so the reported error is stable.
	for _, required := range []struct{ key, value string }{
		{"MULTICA_FLEET_NAMESPACE", cfg.Namespace},
		{"MULTICA_FLEET_SECRET_FILE", cfg.SecretFile},
		{"MULTICA_FLEET_DOCKER_HOST", cfg.DockerHost},
		{"MULTICA_FLEET_NODE_NETWORK", cfg.NodeNetwork},
		{"MULTICA_FLEET_RUNTIME_IMAGE", cfg.RuntimeImage},
		{"MULTICA_FLEET_API_URL", cfg.APIURL},
		{"DATABASE_URL", cfg.DatabaseURL},
		{"MULTICA_FLEET_CREDENTIALS_FILE", cfg.CredentialsFile},
	} {
		if required.value == "" {
			return Config{}, fmt.Errorf("%s is required", required.key)
		}
	}

	if err := validateListen(cfg.Listen, get(containerEnv) == "1"); err != nil {
		return Config{}, err
	}

	if raw := get("MULTICA_FLEET_MAX_NODES_PER_OWNER"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("MULTICA_FLEET_MAX_NODES_PER_OWNER must be a positive integer, got %q", raw)
		}
		cfg.MaxNodesPerOwner = n
	}

	info, err := os.Stat(cfg.SecretFile)
	if err != nil {
		return Config{}, fmt.Errorf("fleet secret file is unreadable: %w", err)
	}
	if info.IsDir() {
		return Config{}, fmt.Errorf("fleet secret file is not a regular file: %s", cfg.SecretFile)
	}

	return cfg, nil
}

// validateListen accepts loopback only. The single exception is 0.0.0.0 inside
// the Fleet container, where the host port is still published on loopback by
// compose; that requires the explicit MULTICA_FLEET_CONTAINER=1 opt-in.
func validateListen(addr string, inContainer bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("MULTICA_FLEET_LISTEN must be host:port, got %q: %w", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("MULTICA_FLEET_LISTEN has an invalid port, got %q", addr)
	}
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return nil
	case "0.0.0.0":
		if inContainer {
			return nil
		}
		return fmt.Errorf("MULTICA_FLEET_LISTEN must bind loopback (0.0.0.0 requires MULTICA_FLEET_CONTAINER=1), got %q", addr)
	default:
		return fmt.Errorf("MULTICA_FLEET_LISTEN must bind loopback, got %q", addr)
	}
}

// Secret returns the service secret from SecretFile, trimmed of the trailing
// newline files usually carry. The value is never logged or stored in Config.
func (c Config) Secret() (string, error) {
	data, err := os.ReadFile(c.SecretFile)
	if err != nil {
		return "", fmt.Errorf("read fleet secret: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("fleet secret file is empty")
	}
	return secret, nil
}
