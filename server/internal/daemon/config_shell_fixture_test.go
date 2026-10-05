package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ownedLoginShell runs the real discovery script without reading system or user
// login profiles. Its PATH contains only fixture binaries and two tool shims.
func ownedLoginShell(t *testing.T, dirs ...string) string {
	t.Helper()
	return ownedLoginShellWithPrelude(t, "", dirs...)
}

// The prelude contains only test-authored alias or stdout-survivor setup.
func ownedLoginShellWithPrelude(t *testing.T, prelude string, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dirname", "basename"} {
		if err := os.Symlink("/usr/bin/"+name, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range dirs {
		info, err := os.Lstat(dir)
		if err != nil || !filepath.IsAbs(dir) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("discovery PATH must contain owned fixture directories: %q", dir)
		}
	}
	shell := filepath.Join(root, "sh")
	argv := filepath.Join(root, "argv")
	// Single quotes are safe for these test-created paths; fail before execution
	// rather than interpolating an unexpected path into the fixture script.
	fixturePath := strings.Join(append(dirs, tools), string(os.PathListSeparator))
	if strings.ContainsAny(fixturePath+argv, "'\n\r") {
		t.Fatal("unsafe fixture path")
	}
	body := "#!/bin/sh\n" +
		"[ \"$#\" -eq 2 ] && [ \"$1\" = -ilc ] || exit 90\n" +
		"printf '%s\\n' \"$@\" > '" + argv + "'\n" +
		"PATH='" + fixturePath + "'; export PATH\n" +
		"unset ENV BASH_ENV\n" +
		prelude +
		"exec /bin/sh -c \"$2\"\n"
	if err := os.WriteFile(shell, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(shell)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0700 || filepath.Dir(shell) != root {
		t.Fatal("shell must be a private fixture before discovery")
	}
	t.Cleanup(func() {
		raw, err := os.ReadFile(argv)
		if err != nil {
			t.Fatalf("owned login shell was not invoked: %v", err)
		}
		if !strings.HasPrefix(string(raw), "-ilc\nfor n in ") {
			t.Fatalf("unexpected shell argv: %q", raw)
		}
	})
	return shell
}
