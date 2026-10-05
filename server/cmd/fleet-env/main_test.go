package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/fleet/operator"
)

func TestPrivateCommandExplicitFakeAndSanitizedOutput(t *testing.T) {
	dir := t.TempDir()
	paths := map[string]string{}
	for name, raw := range map[string]string{"config.json": `{"namespace":"fixture","fleet_id":"fleet","image":"fake-image","api_url":"http://127.0.0.1:1","specs":{"small":{}}}`, "service-key": "fake-service-marker", "profiles.json": `{"version":1,"owners":{}}`} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	args := []string{"prepare", "--namespace", "fixture", "--fleet-id", "fleet", "--fleet-url", "http://127.0.0.1:1", "--database-url", "postgres://fixture:fake-password@127.0.0.1:1/fixture?sslmode=disable", "--config", paths["config.json"], "--service-key", paths["service-key"], "--profiles", paths["profiles.json"], "--operation-key", "shutdown", "--timeout", "1s"}
	calls := 0
	var out bytes.Buffer
	rc := execute(context.Background(), args, &out, func(_ context.Context, action string, cfg operator.Config) error {
		calls++
		if action != "prepare" || cfg.Namespace != "fixture" || cfg.FleetID != "fleet" {
			t.Fatal("explicit command lost identity")
		}
		return nil
	})
	if rc != 0 || calls != 1 || out.String() != "fleet-env: prepare complete\n" {
		t.Fatalf("fake command=%d %d %q", rc, calls, out.String())
	}
	out.Reset()
	rc = execute(context.Background(), args, &out, func(context.Context, string, operator.Config) error { return errors.New("fake-secret-private-URL") })
	if rc == 0 || out.String() != "fleet-env: denied\n" {
		t.Fatalf("private failure output leaked: %q", out.String())
	}
}

func TestPrivateCommandGateBeforeConstruction(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"quiesce"}, {"status", "--database-url", "postgres://unknown/shared"}, {"destroy", "--timeout", "0s"}} {
		calls := 0
		var out bytes.Buffer
		rc := execute(context.Background(), args, &out, func(context.Context, string, operator.Config) error { calls++; return nil })
		if rc == 0 || calls != 0 {
			t.Fatalf("invalid private inputs reached constructor: rc=%d calls=%d", rc, calls)
		}
		if bytes.Contains(out.Bytes(), []byte("postgres://")) {
			t.Fatal("private argument leaked")
		}
	}
}
