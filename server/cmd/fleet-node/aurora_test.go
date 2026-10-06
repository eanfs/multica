package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/fleet/model"
)

const managedEnrollmentToken = "mse_0123456789abcdef0123456789abcdef01234567"

func writeManagedSecret(t *testing.T, dir string, mode os.FileMode, body string) string {
	t.Helper()
	path := filepath.Join(dir, managedEnrollmentName)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunManagedNodeUsesFixedEnrollment(t *testing.T) {
	dir := t.TempDir()
	path := writeManagedSecret(t, dir, 0o600, managedEnrollmentToken)

	runs := 0
	exec := func(executable string, argv, _ []string) error {
		runs++
		want := []string{daemonExecutable, "daemon", "start", "--managed", "--foreground", "--managed-enrollment-token-file=" + path}
		if executable != daemonExecutable || !reflect.DeepEqual(argv, want) {
			t.Fatalf("managed exec = %q %v", executable, argv)
		}
		return nil
	}
	if err := runManagedNode(dir, path, exec); err != nil || runs != 1 {
		t.Fatalf("runManagedNode err=%v runs=%d", err, runs)
	}

	t.Setenv(model.AuroraEnrollmentFileEnv, path)
	if err := runNode(model.NodeHome, dir, "1", exec); err != nil || runs != 2 {
		t.Fatalf("runNode managed profile err=%v runs=%d", err, runs)
	}
}

func TestRunManagedNodeFailsClosed(t *testing.T) {
	dir := t.TempDir()
	good := writeManagedSecret(t, dir, 0o600, managedEnrollmentToken)
	groupReadable := writeManagedSecret(t, dir, 0o640, managedEnrollmentToken)
	malformed := writeManagedSecret(t, dir, 0o600, "not-an-enrollment-token")
	oversized := writeManagedSecret(t, dir, 0o600, managedEnrollmentToken+"\n"+string(make([]byte, 512)))
	missing := filepath.Join(dir, "missing")
	noop := func(string, []string, []string) error { t.Fatal("unsafe enrollment executed"); return nil }

	for name, tc := range map[string]struct{ secrets, path string }{
		"redirected path":  {dir, filepath.Join(t.TempDir(), managedEnrollmentName)},
		"foreign name":     {dir, filepath.Join(dir, "other")},
		"blank secrets":    {"", good},
		"missing file":     {dir, missing},
		"group readable":   {dir, groupReadable},
		"malformed token":  {dir, malformed},
		"oversized secret": {dir, oversized},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runManagedNode(tc.secrets, tc.path, noop); !errors.Is(err, model.ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
