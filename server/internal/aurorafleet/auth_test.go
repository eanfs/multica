package aurorafleet

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testTokenB64 is the base64url form of 32 random-looking printable bytes;
// test fixtures share this one well-formed token. The bytes must be
// printable so the bearer value is a valid HTTP header value.
var (
	testTokenRaw = []byte(strings.Repeat("z", 32))
	testTokenB64 = base64.RawURLEncoding.EncodeToString(testTokenRaw)
)

// writeTokenFile writes a token file with the given mode. content must already
// be the encoded form (base64url or hex).
func writeTokenFile(t *testing.T, dir, encoded string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(encoded), mode); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func TestControlAuthLoadsBase64URLAndHexTokens(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	b64 := base64.RawURLEncoding.EncodeToString(raw)
	hexed := hex.EncodeToString(raw)

	for _, tc := range []struct {
		name    string
		encoded string
	}{{"base64url", b64}, {"hex", hexed}} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTokenFile(t, t.TempDir(), tc.encoded, 0o400)
			auth, err := LoadControlAuth(path)
			if err != nil {
				t.Fatalf("LoadControlAuth: %v", err)
			}
			if string(auth.token) != string(raw) {
				t.Fatalf("decoded token mismatch: got %d bytes", len(auth.token))
			}
		})
	}
}

func TestControlAuthRejectsBadFiles(t *testing.T) {
	b64 := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	cases := []struct {
		name    string
		content string
		mode    os.FileMode
		symlink bool
	}{
		{"wrong mode 0644", b64, 0o644, false},
		{"too large", strings.Repeat("a", 400), 0o400, false},
		{"too short", base64.RawURLEncoding.EncodeToString(make([]byte, 16)), 0o400, false},
		{"not encoded", strings.Repeat("!", 64), 0o400, false},
		{"symlink", b64, 0o400, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTokenFile(t, dir, tc.content, tc.mode)
			if tc.symlink {
				link := filepath.Join(dir, "link")
				if err := os.Symlink(path, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				path = link
			}
			if _, err := LoadControlAuth(path); err == nil {
				t.Fatal("LoadControlAuth accepted an invalid token file")
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadControlAuth(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Fatal("LoadControlAuth accepted a missing file")
		}
	})
}

func newAuthForTest(t *testing.T) *ControlAuth {
	t.Helper()
	path := writeTokenFile(t, t.TempDir(), testTokenB64, 0o400)
	auth, err := LoadControlAuth(path)
	if err != nil {
		t.Fatalf("LoadControlAuth: %v", err)
	}
	return auth
}

func TestControlAuthIdentical401ForMissingAndWrong(t *testing.T) {
	auth := newAuthForTest(t)

	do := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/workspace-nodes/x", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		return rec
	}

	missing := do("")
	wrong := do("dG9rZW4")
	if missing.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("status codes: missing=%d wrong=%d, want 401/401", missing.Code, wrong.Code)
	}
	if missing.Body.String() != wrong.Body.String() {
		t.Fatalf("responses differ: missing=%q wrong=%q", missing.Body.String(), wrong.Body.String())
	}
	if got := do("invalid-base64!!").Code; got != http.StatusUnauthorized {
		t.Fatalf("malformed bearer = %d, want 401", got)
	}
	if got := do("malformed").Code; got != http.StatusUnauthorized {
		t.Fatalf("non-bearer header = %d, want 401", got)
	}

	// The real token passes.
	if got := do(string(testTokenRaw)).Code; got != http.StatusOK {
		t.Fatalf("valid bearer = %d, want 200", got)
	}
}
