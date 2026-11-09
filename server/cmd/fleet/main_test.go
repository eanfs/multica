package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// validEnv returns a complete Fleet environment backed by a temporary secrets
// directory. These tests never contact Docker or PostgreSQL.
func validEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "fleet-secret")
	if err := os.WriteFile(secretPath, []byte("test-secret\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	return map[string]string{
		"MULTICA_FLEET_NAMESPACE":        "wt-test",
		"MULTICA_FLEET_SECRET_FILE":      secretPath,
		"MULTICA_FLEET_DOCKER_HOST":      "unix:///var/run/docker.sock",
		"MULTICA_FLEET_NODE_NETWORK":     "multica-fleet-wt-test",
		"MULTICA_FLEET_RUNTIME_IMAGE":    "multica/runtime-claude:test",
		"MULTICA_FLEET_API_URL":          "http://host.docker.internal:8080",
		"DATABASE_URL":                   "postgres://multica:multica@localhost:5432/multica?sslmode=disable",
		"MULTICA_FLEET_CREDENTIALS_FILE": filepath.Join(dir, "fleet-credentials.json"),
	}
}

func envFunc(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadStartupConfigReturnsTrimmedSecret(t *testing.T) {
	env := validEnv(t)
	cfg, secret, err := loadStartupConfig(envFunc(env))
	if err != nil {
		t.Fatalf("loadStartupConfig: %v", err)
	}
	if secret != "test-secret" {
		t.Fatalf("secret = %q, want the trimmed file contents", secret)
	}
	if cfg.Namespace != "wt-test" {
		t.Fatalf("namespace = %q, want wt-test", cfg.Namespace)
	}
}

func TestLoadStartupConfigRejectsEmptySecret(t *testing.T) {
	env := validEnv(t)
	if err := os.WriteFile(env["MULTICA_FLEET_SECRET_FILE"], []byte("  \n"), 0o600); err != nil {
		t.Fatalf("rewrite secret: %v", err)
	}
	_, _, err := loadStartupConfig(envFunc(env))
	if err == nil {
		t.Fatal("loadStartupConfig accepted an empty secret file")
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %q, want it to name the secret", err)
	}
}

// TestNewPoolInvalidURLDoesNotEchoDSN pins the startup log contract: an
// unparseable DATABASE_URL must not put credentials in the error a caller logs.
func TestNewPoolInvalidURLDoesNotEchoDSN(t *testing.T) {
	const dsn = "postgres://user:super-secret-password@[::1:5432/db"
	pool, err := newPool(context.Background(), dsn)
	if pool != nil {
		pool.Close()
	}
	if err == nil {
		t.Fatal("newPool accepted an invalid DATABASE_URL")
	}
	if strings.Contains(err.Error(), "super-secret-password") || strings.Contains(err.Error(), "[::1") {
		t.Fatal("newPool error echoed the raw DATABASE_URL")
	}
}

// TestNewPoolConfigBoundsConnectionsAndTimeouts pins the process-level limits:
// an unbounded pool plus no statement timeout would let one stuck coordinator
// query occupy a database connection forever.
func TestNewPoolConfigBoundsConnectionsAndTimeouts(t *testing.T) {
	cfg, err := newPoolConfig("postgres://multica:multica@localhost:5432/multica?sslmode=disable")
	if err != nil {
		t.Fatalf("newPoolConfig: %v", err)
	}
	if cfg.MaxConns != poolMaxConns || cfg.MinConns != poolMinConns {
		t.Fatalf("pool conns = %d/%d, want %d/%d", cfg.MinConns, cfg.MaxConns, poolMinConns, poolMaxConns)
	}
	if cfg.MaxConns <= 0 || cfg.MinConns < 0 || cfg.MinConns > cfg.MaxConns {
		t.Fatalf("pool bounds are invalid: min=%d max=%d", cfg.MinConns, cfg.MaxConns)
	}
	params := cfg.ConnConfig.RuntimeParams
	if params["statement_timeout"] != strconv.FormatInt(statementTimeout.Milliseconds(), 10) {
		t.Fatalf("statement_timeout = %q", params["statement_timeout"])
	}
	if params["lock_timeout"] != strconv.FormatInt(lockTimeout.Milliseconds(), 10) {
		t.Fatalf("lock_timeout = %q", params["lock_timeout"])
	}
}

func TestNewHTTPServerSetsHeaderAndIdleTimeouts(t *testing.T) {
	server := newHTTPServer("127.0.0.1:8091", http.NotFoundHandler())
	if server.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout must be bounded")
	}
	if server.IdleTimeout <= 0 {
		t.Fatal("IdleTimeout must be bounded")
	}
}
