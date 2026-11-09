package cloudruntime

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveCloudRuntimeConfig covers the local-fleet configuration matrix.
// The resolver is the only place that turns environment variables plus one
// secret file into the runtime wiring, so every rejection path is pinned here.
// It lives in this package (not cmd/server) so it runs without a database.
func TestResolveCloudRuntimeConfig(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "fleet-secret")
	if err := os.WriteFile(secretPath, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(dir, "empty-secret")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	blankPath := filepath.Join(dir, "blank-secret")
	if err := os.WriteFile(blankPath, []byte(" \n\t\r\n  "), 0o600); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(dir, "missing-secret")

	cases := []struct {
		name          string
		env           map[string]string
		wantErr       bool
		wantEnabled   bool
		wantURL       string
		wantSecret    string
		wantNamespace string
	}{
		{
			name: "neither configured keeps self-hosted behaviour",
			env:  map[string]string{},
		},
		{
			name: "cloud only stays SaaS",
			env:  map[string]string{"MULTICA_CLOUD_URL": "http://cloud"},
		},
		{
			name:    "local and cloud are mutually exclusive",
			env:     map[string]string{"MULTICA_CLOUD_URL": "http://cloud", "MULTICA_LOCAL_FLEET_URL": "http://fleet"},
			wantErr: true,
		},
		{
			name: "blank local URL behaves as unset",
			env:  map[string]string{"MULTICA_LOCAL_FLEET_URL": "  \n\t ", "MULTICA_LOCAL_FLEET_NAMESPACE": "ns"},
		},
		{
			name: "blank local URL with cloud keeps SaaS",
			env:  map[string]string{"MULTICA_CLOUD_URL": "http://cloud", "MULTICA_LOCAL_FLEET_URL": "   "},
		},
		{
			name:    "local without secret file",
			env:     map[string]string{"MULTICA_LOCAL_FLEET_URL": "http://fleet", "MULTICA_LOCAL_FLEET_NAMESPACE": "ns"},
			wantErr: true,
		},
		{
			name:    "local with blank secret file path",
			env:     map[string]string{"MULTICA_LOCAL_FLEET_URL": "http://fleet", "MULTICA_LOCAL_FLEET_SECRET_FILE": "  ", "MULTICA_LOCAL_FLEET_NAMESPACE": "ns"},
			wantErr: true,
		},
		{
			name:    "local with malformed URL",
			env:     map[string]string{"MULTICA_LOCAL_FLEET_URL": "file:///etc/passwd", "MULTICA_LOCAL_FLEET_SECRET_FILE": secretPath, "MULTICA_LOCAL_FLEET_NAMESPACE": "ns"},
			wantErr: true,
		},
		{
			name:    "local with URL query is rejected",
			env:     map[string]string{"MULTICA_LOCAL_FLEET_URL": "http://fleet?x=1", "MULTICA_LOCAL_FLEET_SECRET_FILE": secretPath, "MULTICA_LOCAL_FLEET_NAMESPACE": "ns"},
			wantErr: true,
		},
		{
			name:    "local without namespace",
			env:     map[string]string{"MULTICA_LOCAL_FLEET_URL": "http://fleet", "MULTICA_LOCAL_FLEET_SECRET_FILE": secretPath},
			wantErr: true,
		},
		{
			name:    "local with unreadable secret file",
			env:     map[string]string{"MULTICA_LOCAL_FLEET_URL": "http://fleet", "MULTICA_LOCAL_FLEET_SECRET_FILE": missingPath, "MULTICA_LOCAL_FLEET_NAMESPACE": "ns"},
			wantErr: true,
		},
		{
			name:    "local with empty secret file",
			env:     map[string]string{"MULTICA_LOCAL_FLEET_URL": "http://fleet", "MULTICA_LOCAL_FLEET_SECRET_FILE": emptyPath, "MULTICA_LOCAL_FLEET_NAMESPACE": "ns"},
			wantErr: true,
		},
		{
			name:    "local with whitespace-only secret file",
			env:     map[string]string{"MULTICA_LOCAL_FLEET_URL": "http://fleet", "MULTICA_LOCAL_FLEET_SECRET_FILE": blankPath, "MULTICA_LOCAL_FLEET_NAMESPACE": "ns"},
			wantErr: true,
		},
		{
			name:          "local valid trims whitespace",
			env:           map[string]string{"MULTICA_LOCAL_FLEET_URL": "  http://fleet  ", "MULTICA_LOCAL_FLEET_SECRET_FILE": "  " + secretPath + "  ", "MULTICA_LOCAL_FLEET_NAMESPACE": "  ns  "},
			wantEnabled:   true,
			wantURL:       "http://fleet",
			wantSecret:    "s3cret",
			wantNamespace: "ns",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ResolveLocalFleetConfig(func(key string) string { return tc.env[key] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if cfg.LocalEnabled != tc.wantEnabled {
				t.Fatalf("LocalEnabled = %v, want %v", cfg.LocalEnabled, tc.wantEnabled)
			}
			if cfg.LocalURL != tc.wantURL {
				t.Fatalf("LocalURL = %q, want %q", cfg.LocalURL, tc.wantURL)
			}
			if cfg.LocalSecret != tc.wantSecret {
				t.Fatalf("LocalSecret = %q, want %q", cfg.LocalSecret, tc.wantSecret)
			}
			if cfg.LocalNamespace != tc.wantNamespace {
				t.Fatalf("LocalNamespace = %q, want %q", cfg.LocalNamespace, tc.wantNamespace)
			}
		})
	}
}
