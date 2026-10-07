package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// writeSeccompProfile writes one operator-owned profile and returns its path.
func writeSeccompProfile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seccomp.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write seccomp profile: %v", err)
	}
	return path
}

// TestResolveAuroraSeccompInlinesCompactJSON pins the D2 contract: only the
// profile content reaches HostConfig, and valid JSON becomes compact inline JSON.
func TestResolveAuroraSeccompInlinesCompactJSON(t *testing.T) {
	inline, err := resolveAuroraSeccomp(nil)
	if err != nil || inline != "" {
		t.Fatalf("nil profile = %q, %v; want empty and no error", inline, err)
	}

	pretty := "{\n  \"defaultAction\": \"SCMP_ACT_ALLOW\",\n  \"syscalls\": []\n}\n"
	got, err := resolveAuroraSeccomp(&model.AuroraConfig{SeccompProfile: writeSeccompProfile(t, pretty)})
	if err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	want := "{\"defaultAction\":\"SCMP_ACT_ALLOW\",\"syscalls\":[]}"
	if got != want {
		t.Fatalf("inline profile = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, " \t\n") || strings.Contains(got, "/") {
		t.Fatalf("inline profile is not compact JSON: %q", got)
	}
}

// TestLoadSeccompProfileFailsClosed proves a missing, non-regular, malformed,
// empty or oversized operator profile is never silently accepted.
func TestLoadSeccompProfileFailsClosed(t *testing.T) {
	oversize := "{\"defaultAction\":\"SCMP_ACT_ALLOW\"," + string(make([]byte, maxSeccompProfileBytes)) + "}"
	cases := []struct {
		name    string
		path    string
		wantErr error
	}{
		{"missing file", filepath.Join(t.TempDir(), "absent.json"), model.ErrUnavailable},
		{"directory", t.TempDir(), model.ErrInvalidRequest},
		{"empty file", writeSeccompProfile(t, ""), model.ErrInvalidRequest},
		{"malformed json", writeSeccompProfile(t, "{not json"), model.ErrInvalidRequest},
		{"json array", writeSeccompProfile(t, "[1,2]"), model.ErrInvalidRequest},
		{"json null", writeSeccompProfile(t, "null"), model.ErrInvalidRequest},
		{"missing defaultAction", writeSeccompProfile(t, "{\"syscalls\":[]}"), model.ErrInvalidRequest},
		{"oversized", writeSeccompProfile(t, oversize), model.ErrInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadSeccompProfile(tc.path); !errors.Is(err, tc.wantErr) {
				t.Fatalf("loadSeccompProfile(%s) = %v, want %v", tc.name, err, tc.wantErr)
			}
		})
	}
}

// TestEnsureRejectsBadSeccompBeforeAnyResource proves a bad operator profile
// fails closed with no Docker side effect, so a missing profile can never yield
// a silently weaker container.
func TestEnsureRejectsBadSeccompBeforeAnyResource(t *testing.T) {
	cases := []struct {
		name    string
		path    func(t *testing.T) string
		wantErr error
	}{
		{"missing profile", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") }, model.ErrUnavailable},
		{"invalid profile", func(t *testing.T) string { return writeSeccompProfile(t, "{not json") }, model.ErrInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := auroraConfig()
			cfg.Aurora.SeccompProfile = tc.path(t)
			n := auroraNode()
			calls := 0
			e := fakeCalls{
				inspect:       func(context.Context, string) (Inspection, error) { return Inspection{}, errdefs.ErrNotFound },
				ensureNetwork: func(context.Context, Resource) error { calls++; return nil },
				ensureVolume:  func(context.Context, Resource) error { calls++; return nil },
				bootstrap:     func(context.Context, []Resource, []byte) error { calls++; return nil },
				create: func(context.Context, *container.Config, *container.HostConfig, string, string) (string, error) {
					calls++
					return "", nil
				},
			}
			if _, err := New(e, cfg).Ensure(context.Background(), n, auroraBootstrap(n)); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Ensure = %v, want %v", err, tc.wantErr)
			}
			if calls != 0 {
				t.Fatalf("Docker side effects before seccomp validation: %d", calls)
			}
		})
	}
}
