package fleet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCredentials(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveUsesOwnerMapping(t *testing.T) {
	path := writeCredentials(t, `{"users":{"owner-1":"work"},"profiles":{"work":{"env":{"ANTHROPIC_API_KEY":"sk-test","ANTHROPIC_BASE_URL":"https://example.test"}}}}`)
	creds, err := LoadCredentials(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	profile, err := creds.Resolve("owner-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if profile.Env["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Fatalf("unexpected env: %+v", profile.Env)
	}
	if profile.Env["ANTHROPIC_BASE_URL"] != "https://example.test" {
		t.Fatalf("unexpected env: %+v", profile.Env)
	}
}

// TestResolveHasNoDefaultProfile pins the fail-closed rule: an owner without an
// explicit mapping never falls back to a shared profile.
func TestResolveHasNoDefaultProfile(t *testing.T) {
	creds, err := LoadCredentials(writeCredentials(t, `{"users":{},"profiles":{"default":{"env":{"ANTHROPIC_API_KEY":"sk-test"}}}}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := creds.Resolve("owner-1"); !errors.Is(err, ErrProfileMissing) {
		t.Fatalf("err = %v, want ErrProfileMissing", err)
	}
}

func TestResolveRejectsMissingOwnerAndUnknownProfile(t *testing.T) {
	creds, err := LoadCredentials(writeCredentials(t, `{"users":{},"profiles":{}}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := creds.Resolve("owner-1"); !errors.Is(err, ErrProfileMissing) {
		t.Fatalf("err = %v, want ErrProfileMissing", err)
	}

	creds, err = LoadCredentials(writeCredentials(t, `{"users":{"owner-1":"ghost"},"profiles":{}}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := creds.Resolve("owner-1"); !errors.Is(err, ErrProfileInvalid) {
		t.Fatalf("err = %v, want ErrProfileInvalid", err)
	}
}

func TestResolveTreatsEmptyMappingAsMissing(t *testing.T) {
	creds, err := LoadCredentials(writeCredentials(t, `{"users":{"owner-1":"","owner-2":"work"},"profiles":{"work":{"env":{"ANTHROPIC_API_KEY":"sk-test"}}}}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := creds.Resolve("owner-1"); !errors.Is(err, ErrProfileMissing) {
		t.Fatalf("err = %v, want ErrProfileMissing", err)
	}
	if _, err := creds.Resolve("owner-2"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
}

// TestResolveReturnsIsolatedEnv keeps one caller from mutating the loaded
// credential set, which owners sharing a profile would observe.
func TestResolveReturnsIsolatedEnv(t *testing.T) {
	creds, err := LoadCredentials(writeCredentials(t, `{"users":{"owner-1":"work","owner-2":"work"},"profiles":{"work":{"env":{"ANTHROPIC_API_KEY":"sk-test"}}}}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	first, err := creds.Resolve("owner-1")
	if err != nil {
		t.Fatalf("resolve owner-1: %v", err)
	}
	first.Env["NODE_ID"] = "injected"

	second, err := creds.Resolve("owner-2")
	if err != nil {
		t.Fatalf("resolve owner-2: %v", err)
	}
	if _, ok := second.Env["NODE_ID"]; ok {
		t.Fatalf("env mutation leaked across resolves: %+v", second.Env)
	}
	again, err := creds.Resolve("owner-1")
	if err != nil {
		t.Fatalf("resolve owner-1 again: %v", err)
	}
	if _, ok := again.Env["NODE_ID"]; ok {
		t.Fatalf("env mutation persisted in loaded credentials: %+v", again.Env)
	}
}

func TestLoadCredentialsAcceptsAllowlistedEnv(t *testing.T) {
	path := writeCredentials(t, `{"users":{"owner-1":"work"},"profiles":{"work":{"env":{
		"ANTHROPIC_API_KEY":"sk-test",
		"ANTHROPIC_AUTH_TOKEN":"token",
		"ANTHROPIC_BASE_URL":"https://example.test",
		"ANTHROPIC_MODEL":"claude-sonnet-4-5",
		"CLAUDE_CODE_USE_BEDROCK":"1",
		"API_TIMEOUT_MS":"600000"
	}}}}`)
	creds, err := LoadCredentials(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	profile, err := creds.Resolve("owner-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(profile.Env) != 6 {
		t.Fatalf("got %d env entries, want 6: %+v", len(profile.Env), profile.Env)
	}
}

func TestLoadCredentialsAcceptsValueAtLengthLimit(t *testing.T) {
	body := `{"users":{"owner-1":"work"},"profiles":{"work":{"env":{"ANTHROPIC_API_KEY":"` + strings.Repeat("a", 8192) + `"}}}}`
	if _, err := LoadCredentials(writeCredentials(t, body)); err != nil {
		t.Fatalf("load at limit: %v", err)
	}
}

func TestLoadCredentialsRejectsInjectedOrUnknownEnv(t *testing.T) {
	const marker = "sk-secret-must-not-leak"
	cases := []struct {
		name string
		body string
		// decodedValue, when set, must appear in the decoded JSON value. It
		// proves the fixture really delivers the payload, so a rejection cannot
		// pass vacuously on a value that never contained it.
		decodedValue string
	}{
		{
			name: "unknown env name",
			body: `{"users":{"o":"p"},"profiles":{"p":{"env":{"MULTICA_TOKEN":"` + marker + `"}}}}`,
		},
		{
			name: "PATH is not a model variable",
			body: `{"users":{"o":"p"},"profiles":{"p":{"env":{"PATH":"/tmp/bin"}}}}`,
		},
		{
			name: "env key is case sensitive",
			body: `{"users":{"o":"p"},"profiles":{"p":{"env":{"anthropic_api_key":"` + marker + `"}}}}`,
		},
		{
			name:         "newline in value",
			body:         `{"users":{"o":"p"},"profiles":{"p":{"env":{"ANTHROPIC_API_KEY":"` + marker + `\nMULTICA_TOKEN=x"}}}}`,
			decodedValue: "\n",
		},
		{
			name:         "carriage return in value",
			body:         `{"users":{"o":"p"},"profiles":{"p":{"env":{"ANTHROPIC_API_KEY":"` + marker + `\rMULTICA_TOKEN=x"}}}}`,
			decodedValue: "\r",
		},
		{
			name:         "NUL in value",
			body:         `{"users":{"o":"p"},"profiles":{"p":{"env":{"ANTHROPIC_API_KEY":"` + marker + `\u0000MULTICA_TOKEN=x"}}}}`,
			decodedValue: "\x00",
		},
		{
			name: "value too long",
			body: `{"users":{"o":"p"},"profiles":{"p":{"env":{"ANTHROPIC_API_KEY":"` + strings.Repeat("b", 8193) + `"}}}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.decodedValue != "" {
				var decoded struct {
					Profiles map[string]Profile `json:"profiles"`
				}
				if err := json.Unmarshal([]byte(tc.body), &decoded); err != nil {
					t.Fatalf("fixture is not valid JSON: %v", err)
				}
				value := decoded.Profiles["p"].Env["ANTHROPIC_API_KEY"]
				if !strings.Contains(value, tc.decodedValue) {
					t.Fatalf("fixture did not decode to %q, got %q", tc.decodedValue, value)
				}
			}

			_, err := LoadCredentials(writeCredentials(t, tc.body))
			if err == nil {
				t.Fatal("expected rejection")
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), strings.Repeat("b", 8193)) {
				t.Fatalf("error leaked a secret value: %v", err)
			}
		})
	}
}

func TestLoadCredentialsRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty file", ""},
		{"not json", "users:"},
		{"wrong type", `{"users":{"o":1}}`},
		{"trailing data", `{"users":{},"profiles":{}}x`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadCredentials(writeCredentials(t, tc.body)); err == nil {
				t.Fatalf("expected rejection for %q", tc.body)
			}
		})
	}
}

func TestLoadCredentialsMissingFile(t *testing.T) {
	_, err := LoadCredentials(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want a not-exist error", err)
	}
}
