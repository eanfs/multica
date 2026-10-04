package model

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `{"namespace":"local","fleet_id":"fleet-1","image":"trusted/image:dev","api_url":"http://127.0.0.1:8080","specs":{"small":{}}}`

func loadTestConfig(t *testing.T, raw string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.json")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadTestConfig(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != "local" || cfg.FleetID != "fleet-1" || cfg.Image != "trusted/image:dev" || cfg.APIURL != "http://127.0.0.1:8080" {
		t.Fatalf("administrator config lost: %+v", cfg)
	}
	if cfg.MaxNodes != 2 {
		t.Fatalf("MaxNodes = %d, want 2", cfg.MaxNodes)
	}
	if got := cfg.Specs["small"]; got != (Spec{CPUs: 2, MemoryBytes: 4294967296, Pids: 256, MaxRuns: 1}) {
		t.Fatalf("default spec = %+v", got)
	}
}

func TestLoadConfigExplicitResources(t *testing.T) {
	raw := strings.Replace(validConfig, `"small":{}`, `"small":{"cpus":3,"memory_bytes":8589934592,"pids":512,"max_runs":2}`, 1)
	raw = strings.Replace(raw, `"namespace":"local"`, `"max_nodes":4,"namespace":"local"`, 1)
	cfg, err := loadTestConfig(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxNodes != 4 || cfg.Specs["small"] != (Spec{CPUs: 3, MemoryBytes: 8589934592, Pids: 512, MaxRuns: 2}) {
		t.Fatalf("explicit limits lost: %+v", cfg)
	}
}

func TestLoadConfigRejectsUnsafeOrMalformedFields(t *testing.T) {
	cases := map[string]string{
		"unknown image override": strings.Replace(validConfig, `"namespace"`, `"node_image":"evil","namespace"`, 1),
		"unknown path":           strings.Replace(validConfig, `"namespace"`, `"data_path":"/host","namespace"`, 1),
		"nested image":           strings.Replace(validConfig, `"small":{}`, `"small":{"image":"evil"}`, 1),
		"nested path":            strings.Replace(validConfig, `"small":{}`, `"small":{"path":"/host"}`, 1),
		"profile secrets":        strings.Replace(validConfig, `"namespace"`, `"api_key":"test-only","namespace"`, 1),
		"missing namespace":      strings.Replace(validConfig, `"namespace":"local",`, "", 1),
		"missing fleet":          strings.Replace(validConfig, `"fleet_id":"fleet-1",`, "", 1),
		"missing image":          strings.Replace(validConfig, `"image":"trusted/image:dev",`, "", 1),
		"missing API URL":        strings.Replace(validConfig, `"api_url":"http://127.0.0.1:8080",`, "", 1),
		"no specs":               strings.Replace(validConfig, `"small":{}`, "", 1),
		"blank spec name":        strings.Replace(validConfig, `"small":{}`, `" ":{}`, 1),
		"case variant identity":  strings.Replace(validConfig, `"namespace"`, `"Namespace"`, 1),
		"case variant resource":  strings.Replace(validConfig, `"small":{}`, `"small":{"CPUs":2}`, 1),
		"duplicate image":        strings.Replace(validConfig, `"image":"trusted/image:dev"`, `"image":"evil","image":"trusted/image:dev"`, 1),
		"duplicate resource":     strings.Replace(validConfig, `"small":{}`, `"small":{"cpus":0,"cpus":2}`, 1),
		"duplicate spec":         strings.Replace(validConfig, `"small":{}`, `"small":{"cpus":0},"small":{}`, 1),
		"malformed":              "{",
		"trailing object":        validConfig + "{}",
		"null config":            "null",
		"null spec":              strings.Replace(validConfig, `"small":{}`, `"small":null`, 1),
	}
	for _, field := range []string{"cpus", "memory_bytes", "pids", "max_runs"} {
		for _, value := range []string{"0", "-1", "null"} {
			cases[field+" "+value] = strings.Replace(validConfig, `"small":{}`, `"small":{"`+field+`":`+value+`}`, 1)
		}
	}
	for _, value := range []string{"0", "-1", "null"} {
		cases["max_nodes "+value] = strings.Replace(validConfig, `"namespace"`, `"max_nodes":`+value+`,"namespace"`, 1)
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadTestConfig(t, raw)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("LoadConfig error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want missing file", err)
	}
}
