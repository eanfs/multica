package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// BootstrapFiles consumes installer-owned files; it never produces fleet identity.
// It sets HOME only to the validated node home for the existing default-profile saver.
func BootstrapFiles(home, secretDir string) (cli.CLIConfig, error) {
	cfg, _, err := bootstrapFiles(home, secretDir)
	return cfg, err
}
func bootstrapFiles(home, secretDir string) (cli.CLIConfig, model.Bootstrap, error) {
	fail := func() (cli.CLIConfig, model.Bootstrap, error) {
		return cli.CLIConfig{}, model.Bootstrap{}, model.ErrInvalidRequest
	}
	if !filepath.IsAbs(home) || filepath.Base(home) != "home" || !filepath.IsAbs(secretDir) || os.Getenv(cli.TaskConfigRootEnv) != "" {
		return fail()
	}
	layout, _, err := readLayout(filepath.Dir(home))
	if err != nil || privateDirectory(secretDir, 0700) != nil {
		return fail()
	}
	raw, err := readPrivateFile(filepath.Join(secretDir, "bootstrap.json"))
	if err != nil {
		return fail()
	}
	// Bootstrap's optional tags intentionally do not form an exact-key schema.
	var fields map[string]json.RawMessage
	if _, err := model.DecodeStrictObject(raw, &fields); err != nil {
		return fail()
	}
	allowed := map[string]bool{"node_token": true, "api_key": true, "base_url": true, "model": true, "server_url": true, "daemon_id": true}
	for key := range fields {
		if !allowed[key] {
			return fail()
		}
	}
	var b model.Bootstrap
	if json.Unmarshal(raw, &b) != nil {
		return fail()
	}
	token, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(b.NodeToken, "mcn_"))
	if err != nil || len(token) != 32 || b.NodeToken != "mcn_"+base64.RawURLEncoding.EncodeToString(token) || !validUUID(b.DaemonID) || b.DaemonID != layout.DaemonID || !validSecret(b.APIKey) || !validURL(b.ServerURL) {
		return fail()
	}
	if _, ok := fields["base_url"]; ok && !validURL(b.BaseURL) {
		return fail()
	}
	if _, ok := fields["model"]; ok && !validSecret(b.Model) {
		return fail()
	}
	configDir := filepath.Join(home, ".multica")
	configPath := filepath.Join(configDir, "config.json")
	if info, err := os.Lstat(configDir); err == nil {
		if !info.IsDir() || !owned(info) || info.Mode().Perm()&0022 != 0 {
			return fail()
		}
	} else if !os.IsNotExist(err) {
		return fail()
	}
	cfg := cli.CLIConfig{Token: b.NodeToken, ServerURL: b.ServerURL, WorkspacesRoot: model.WorkspacesRoot}
	if _, err := os.Lstat(configPath); err == nil {
		raw, err := readPrivateFile(configPath)
		if err != nil {
			return fail()
		}
		var existing cli.CLIConfig
		var fields map[string]json.RawMessage
		if _, err := model.DecodeStrictObject(raw, &fields); err != nil {
			return fail()
		}
		if json.Unmarshal(raw, &existing) != nil || existing.Token != b.NodeToken || existing.ServerURL != b.ServerURL || (existing.WorkspacesRoot != "" && existing.WorkspacesRoot != model.WorkspacesRoot) {
			return fail()
		}
		if os.Setenv("HOME", home) != nil {
			return fail()
		}
		return existing, b, nil // Preserve the exact existing profile and all node data.
	} else if !os.IsNotExist(err) {
		return fail()
	}
	if os.Setenv("HOME", home) != nil || cli.SaveCLIConfigForProfile(cfg, "") != nil {
		return fail()
	}
	return cfg, b, nil
}
func validSecret(value string) bool {
	return strings.TrimSpace(value) != "" && strings.IndexFunc(value, unicode.IsControl) < 0
}
func validURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && validSecret(value) && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.Contains(value, "#")
}
