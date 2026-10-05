package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/pkg/agent"
)

const testDaemonID = "00000000-0000-4000-8000-000000000003"
const testNodeToken = "mcn_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func nodeFixture(t *testing.T) (string, string, string) {
	t.Helper()
	data := t.TempDir()
	home := filepath.Join(data, "home")
	secrets := t.TempDir()
	for _, p := range []string{data, secrets} {
		if err := os.Chmod(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{home, filepath.Join(data, "workspaces")} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	manifest := model.LayoutManifestData{Version: 1, Namespace: "unit", FleetID: "unit-fleet", NodeID: "00000000-0000-4000-8000-000000000002", DaemonID: testDaemonID, DataMount: "/data", NodeHome: "/data/home", WorkspacesRoot: "/data/workspaces"}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "fleet-layout.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	raw = []byte(`{"node_token":"mcn_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","server_url":"http://api.test","daemon_id":"00000000-0000-4000-8000-000000000003","api_key":"unit-test-only","base_url":"https://provider.test","model":"unit-model"}`)
	if err := os.WriteFile(filepath.Join(secrets, "bootstrap.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(cli.TaskConfigRootEnv, "")
	t.Setenv("MULTICA_TOKEN", "")
	return data, home, secrets
}

func TestBootstrapRejectsUnsafeIdentityAndPreservesData(t *testing.T) {
	for _, kind := range []string{"owner-profile", "server-profile", "missing-profile-identity", "manifest-daemon", "manifest-path", "manifest-missing", "secret-mode", "secret-symlink", "home-symlink", "task-config-override", "invalid-token", "invalid-uuid", "extra-secret", "malformed-secret"} {
		t.Run(kind, func(t *testing.T) {
			data, home, secrets := nodeFixture(t)
			sentinel := filepath.Join(home, "session")
			if err := os.WriteFile(sentinel, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(home, ".multica", "config.json")
			if err := os.Mkdir(filepath.Dir(configPath), 0700); err != nil {
				t.Fatal(err)
			}
			original := []byte(`{"token":"mcn_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","server_url":"http://api.test","device_name":"keep"}`)
			if kind == "owner-profile" {
				original = []byte(`{"token":"owner-token","server_url":"http://api.test"}`)
			}
			if kind == "missing-profile-identity" {
				original = []byte(`{"device_name":"owner profile without credentials"}`)
			}
			if kind == "server-profile" {
				original = []byte(`{"token":"mcn_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","server_url":"http://other.test"}`)
			}
			if err := os.WriteFile(configPath, original, 0600); err != nil {
				t.Fatal(err)
			}
			secretPath := filepath.Join(secrets, "bootstrap.json")
			manifestPath := filepath.Join(data, "fleet-layout.json")
			switch kind {
			case "manifest-daemon", "manifest-path":
				raw, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				var m model.LayoutManifestData
				checkFixture(t, json.Unmarshal(raw, &m))
				if kind == "manifest-daemon" {
					m.DaemonID = "00000000-0000-4000-8000-000000000009"
				} else {
					m.WorkspacesRoot = "/wrong"
				}
				raw = encodeFixture(t, m)
				checkFixture(t, os.WriteFile(manifestPath, raw, 0600))
			case "manifest-missing":
				checkFixture(t, os.Remove(manifestPath))
			case "secret-mode":
				checkFixture(t, os.Chmod(secretPath, 0644))
			case "secret-symlink":
				target := filepath.Join(secrets, "target")
				checkFixture(t, os.Rename(secretPath, target))
				checkFixture(t, os.Symlink(target, secretPath))
			case "home-symlink":
				target := filepath.Join(data, "home-target")
				// Both paths are test-owned; retain profile and sentinel bytes.
				checkFixture(t, os.Rename(home, target))
				checkFixture(t, os.Symlink(target, home))
				info, err := os.Lstat(home)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("home-symlink fixture is not a symlink")
				}
			case "task-config-override":
				t.Setenv(cli.TaskConfigRootEnv, t.TempDir())
			case "invalid-token", "invalid-uuid", "extra-secret", "malformed-secret":
				raw, err := os.ReadFile(secretPath)
				if err != nil {
					t.Fatal(err)
				}
				var m map[string]any
				checkFixture(t, json.Unmarshal(raw, &m))
				switch kind {
				case "invalid-token":
					m["node_token"] = "mcn_bad"
				case "invalid-uuid":
					m["daemon_id"] = "bad"
				case "extra-secret":
					m["owner_id"] = "no"
				}
				raw = encodeFixture(t, m)
				if kind == "malformed-secret" {
					raw = []byte("unit-test-only")
				}
				checkFixture(t, os.WriteFile(secretPath, raw, 0600))
			}
			if kind == "home-symlink" && filepath.Base(home) != "home" {
				t.Fatal("home-symlink fixture must reach directory validation with basename home")
			}
			_, err := BootstrapFiles(home, secrets)
			if err == nil {
				t.Fatal("unsafe bootstrap accepted")
			}
			if strings.Contains(err.Error(), "unit-test-only") || strings.Contains(err.Error(), testNodeToken) || strings.Contains(err.Error(), secrets) {
				t.Fatal("error leaks private inputs")
			}
			got := readFixture(t, configPath)
			if string(got) != string(original) {
				t.Fatal("existing profile overwritten")
			}
			got = readFixture(t, sentinel)
			if string(got) != "preserved" {
				t.Fatal("node data changed")
			}
		})
	}
}

func TestBootstrapPreservesMatchingProfileAndManifest(t *testing.T) {
	data, home, secrets := nodeFixture(t)
	if err := cli.SaveCLIConfigForProfile(cli.CLIConfig{Token: testNodeToken, ServerURL: "http://api.test", DeviceName: "preserve"}, ""); err != nil {
		t.Fatal(err)
	}
	before := readFixture(t, filepath.Join(home, ".multica", "config.json"))
	manifest := readFixture(t, filepath.Join(data, "fleet-layout.json"))
	cfg, err := BootstrapFiles(home, secrets)
	if err != nil || cfg.DeviceName != "preserve" {
		t.Fatal("matching existing config not retained")
	}
	after := readFixture(t, filepath.Join(home, ".multica", "config.json"))
	afterManifest := readFixture(t, filepath.Join(data, "fleet-layout.json"))
	if string(before) != string(after) || string(manifest) != string(afterManifest) {
		t.Fatal("persistent bytes rewritten")
	}
}

func TestRunUsesFixedForegroundArgvAndProcessOnlyCredentials(t *testing.T) {
	for _, max := range []string{"1", "2"} {
		t.Run(max, func(t *testing.T) {
			_, home, secrets := nodeFixture(t)
			called := false
			sentinel := errors.New("fake exec reached")
			err := runNode(home, secrets, max, func(path string, argv, env []string) error {
				called = true
				want := []string{"/usr/local/bin/multica", "daemon", "start", "--foreground", "--no-auto-update", "--no-auto-reload", "--max-concurrent-tasks", max, "--daemon-id", testDaemonID, "--workspaces-root", "/data/workspaces"}
				if path != "/usr/local/bin/multica" || !reflect.DeepEqual(argv, want) {
					t.Fatalf("fixed argv = %v", argv)
				}
				joined := strings.Join(argv, " ")
				if strings.Contains(joined, "unit-test-only") || strings.Contains(joined, testNodeToken) {
					t.Fatal("credential argv")
				}
				wantEnv := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + home, "ANTHROPIC_API_KEY=unit-test-only", "ANTHROPIC_BASE_URL=https://provider.test", "ANTHROPIC_MODEL=unit-model"}
				if !reflect.DeepEqual(env, wantEnv) {
					t.Fatal("process env not explicit and private")
				}
				return sentinel
			})
			if !called || !errors.Is(err, sentinel) {
				t.Fatalf("fake execution seam not reached: %v", err)
			}
		})
	}
}
func TestRunRejectsInvalidMaxRunsWithoutExecution(t *testing.T) {
	for _, max := range []string{"", "0", "-1", "01", "+2", "2 ", "1.5", "999999999999999999999999999999999999"} {
		t.Run(max, func(t *testing.T) {
			_, home, secrets := nodeFixture(t)
			err := runNode(home, secrets, max, func(string, []string, []string) error { t.Fatal("invalid max runs executed daemon"); return nil })
			if err == nil {
				t.Fatal("invalid max runs accepted")
			}
		})
	}
}

func TestFakeClaudeBackendVersionAndTerminalResultSemantics(t *testing.T) {
	for _, mode := range []string{"success", "error", "missing-result"} {
		t.Run(mode, func(t *testing.T) {
			_, home, _ := nodeFixture(t)
			raw, err := os.ReadFile("../../internal/fleet/integration/testdata/claude")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "claude")
			if err := os.WriteFile(path, raw, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			version, err := agent.DetectVersion(ctx, agent.Command{Path: path})
			if err != nil || version != "2.1.100 (Claude Code)" {
				t.Fatalf("fake version=%q %v", version, err)
			}
			argvPath := filepath.Join(t.TempDir(), "argv")
			backend, err := agent.New("claude", agent.Config{ExecutablePath: path, Env: map[string]string{"HOME": home, "FAKE_CLAUDE_MODE": mode, "FAKE_CLAUDE_ARGV": argvPath, "IS_SANDBOX": "1"}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			session, err := backend.Execute(ctx, "unit-test-only prompt", agent.ExecOptions{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			drained := make(chan struct{})
			go func() {
				for range session.Messages {
				}
				close(drained)
			}()
			select {
			case result := <-session.Result:
				if mode == "success" {
					if result.Status != "completed" || result.Output != "fake Claude completed" || result.SessionID != "00000000-0000-4000-8000-000000000007" {
						t.Fatalf("terminal result lost or stdout adopted: %+v", result)
					}
				} else {
					if result.Status != "failed" || result.Output != "" {
						t.Fatalf("nonterminal/error narration treated as success: %+v", result)
					}
				}
			case <-ctx.Done():
				t.Fatal("fake backend timed out")
			}
			<-drained
			argv, err := os.ReadFile(argvPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range []string{"-p\n", "--output-format\nstream-json\n", "--input-format\nstream-json\n", "--verbose\n"} {
				if !strings.Contains(string(argv), part) {
					t.Fatalf("backend protocol missing %q", part)
				}
			}
		})
	}
}

func TestBootstrapDoesNotNeedMulticaTokenEnv(t *testing.T) {
	_, home, secrets := nodeFixture(t)
	cfg, err := BootstrapFiles(home, secrets)
	if err != nil || cfg.Token != testNodeToken || cfg.ServerURL != "http://api.test" {
		t.Fatalf("bootstrap missing file configuration: %v", err)
	}
	loaded, err := cli.LoadCLIConfigForProfile("")
	if err != nil || loaded.Token != testNodeToken {
		t.Fatal("default-profile config not saved")
	}
	info, err := os.Stat(filepath.Join(home, ".multica", "config.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("config is not private")
	}
}

func checkFixture(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	checkFixture(t, err)
	return raw
}
func encodeFixture(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	checkFixture(t, err)
	return raw
}
