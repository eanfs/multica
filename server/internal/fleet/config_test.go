package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envFrom(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func validEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(creds, []byte("{\"users\":{},\"profiles\":{}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"MULTICA_FLEET_LISTEN":           "127.0.0.1:8091",
		"MULTICA_FLEET_NAMESPACE":        "wt-test",
		"MULTICA_FLEET_SECRET_FILE":      secret,
		"MULTICA_FLEET_DOCKER_HOST":      "unix:///var/run/docker.sock",
		"MULTICA_FLEET_NODE_NETWORK":     "multica-fleet-wt-test",
		"MULTICA_FLEET_RUNTIME_IMAGE":    "multica/runtime-claude:test",
		"MULTICA_FLEET_API_URL":          "http://host.docker.internal:8080",
		"DATABASE_URL":                   "postgres://u:p@postgres:5432/db?sslmode=disable",
		"MULTICA_FLEET_CREDENTIALS_FILE": creds,
	}
}

func TestLoadAcceptsMinimalValidEnvironment(t *testing.T) {
	cfg, err := Load(envFrom(validEnv(t)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8091" || cfg.MaxNodesPerOwner != 2 || len(cfg.Specs) == 0 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Specs[0] != (Spec{ID: "standard", CPU: 2, MemoryMB: 4096, PidsLimit: 256, MaxConcurrent: 1}) {
		t.Fatalf("unexpected default spec: %+v", cfg.Specs[0])
	}
	secret, err := cfg.Secret()
	if err != nil || secret != "s3cret" {
		t.Fatalf("secret = %q, err = %v", secret, err)
	}
}

func TestLoadRejectsMissingAndMalformedValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(map[string]string)
		want string
	}{
		{"missing namespace", func(m map[string]string) { delete(m, "MULTICA_FLEET_NAMESPACE") }, "MULTICA_FLEET_NAMESPACE"},
		{"bad listen", func(m map[string]string) { m["MULTICA_FLEET_LISTEN"] = "0.0.0.0:8091" }, "loopback"},
		{"bad listen port", func(m map[string]string) { m["MULTICA_FLEET_LISTEN"] = "127.0.0.1:notaport" }, "MULTICA_FLEET_LISTEN"},
		{"missing secret file", func(m map[string]string) { m["MULTICA_FLEET_SECRET_FILE"] = "/nonexistent" }, "secret"},
		{"missing database", func(m map[string]string) { delete(m, "DATABASE_URL") }, "DATABASE_URL"},
		{"bad owner limit", func(m map[string]string) { m["MULTICA_FLEET_MAX_NODES_PER_OWNER"] = "0" }, "MAX_NODES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv(t)
			tc.mut(env)
			_, err := Load(envFrom(env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadAllowsContainerListen(t *testing.T) {
	env := validEnv(t)
	env["MULTICA_FLEET_LISTEN"] = "0.0.0.0:8091"

	// Outside a container the container bind address stays forbidden.
	if _, err := Load(envFrom(env)); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("err = %v, want loopback rejection without container mode", err)
	}

	// Only the explicit "1" opt-in, not an arbitrary truthy value, enables it.
	env["MULTICA_FLEET_CONTAINER"] = "true"
	if _, err := Load(envFrom(env)); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("err = %v, want loopback rejection with non-1 container value", err)
	}

	env["MULTICA_FLEET_CONTAINER"] = "1"
	cfg, err := Load(envFrom(env))
	if err != nil {
		t.Fatalf("load with container mode: %v", err)
	}
	if cfg.Listen != "0.0.0.0:8091" {
		t.Fatalf("listen = %q, want 0.0.0.0:8091", cfg.Listen)
	}
}
