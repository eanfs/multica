package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveCloudRuntimeConfig covers the local-fleet configuration matrix.
// The resolver is the only place that turns environment variables plus one
// secret file into the runtime wiring, so every rejection path is pinned here.
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
			name:    "local without secret file",
			env:     map[string]string{"MULTICA_LOCAL_FLEET_URL": "http://fleet", "MULTICA_LOCAL_FLEET_NAMESPACE": "ns"},
			wantErr: true,
		},
		{
			name:    "local with malformed URL",
			env:     map[string]string{"MULTICA_LOCAL_FLEET_URL": "file:///etc/passwd", "MULTICA_LOCAL_FLEET_SECRET_FILE": secretPath, "MULTICA_LOCAL_FLEET_NAMESPACE": "ns"},
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
			name:          "local valid trims whitespace",
			env:           map[string]string{"MULTICA_LOCAL_FLEET_URL": "  http://fleet  ", "MULTICA_LOCAL_FLEET_SECRET_FILE": secretPath, "MULTICA_LOCAL_FLEET_NAMESPACE": "  ns  "},
			wantEnabled:   true,
			wantURL:       "http://fleet",
			wantSecret:    "s3cret",
			wantNamespace: "ns",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := resolveCloudRuntimeConfig(func(key string) string { return tc.env[key] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if cfg.localEnabled != tc.wantEnabled {
				t.Fatalf("localEnabled = %v, want %v", cfg.localEnabled, tc.wantEnabled)
			}
			if cfg.localURL != tc.wantURL {
				t.Fatalf("localURL = %q, want %q", cfg.localURL, tc.wantURL)
			}
			if cfg.localSecret != tc.wantSecret {
				t.Fatalf("localSecret = %q, want %q", cfg.localSecret, tc.wantSecret)
			}
			if cfg.localNamespace != tc.wantNamespace {
				t.Fatalf("localNamespace = %q, want %q", cfg.localNamespace, tc.wantNamespace)
			}
		})
	}
}
