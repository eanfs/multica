package model

import (
	"errors"
	"strings"
	"testing"
)

const auroraConfig = `{"namespace":"local","fleet_id":"fleet-1","image":"trusted/image:dev","api_url":"http://127.0.0.1:8080","specs":{"small":{}},"aurora":{"server_url":"http://api.internal:8080"}}`

func TestLoadConfigAuroraProfile(t *testing.T) {
	cfg, err := loadTestConfig(t, auroraConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Aurora == nil || cfg.Aurora.ServerURL != "http://api.internal:8080" {
		t.Fatalf("aurora profile lost: %+v", cfg.Aurora)
	}
	// A configuration without the profile keeps the default Claude-only node.
	plain, err := loadTestConfig(t, validConfig)
	if err != nil || plain.Aurora != nil {
		t.Fatalf("default profile = %+v err=%v", plain.Aurora, err)
	}
}

func TestLoadConfigRejectsUnsafeAuroraProfile(t *testing.T) {
	cases := map[string]string{
		"missing server url": strings.Replace(auroraConfig, `,"aurora":{"server_url":"http://api.internal:8080"}`, `,"aurora":{}`, 1),
		"empty server url":   strings.Replace(auroraConfig, "http://api.internal:8080", "", 1),
		"credentials in url": strings.Replace(auroraConfig, "http://api.internal:8080", "http://user:pass@api.internal:8080", 1),
		"query in url":       strings.Replace(auroraConfig, "http://api.internal:8080", "http://api.internal:8080/?token=1", 1),
		"path in url":        strings.Replace(auroraConfig, "http://api.internal:8080", "http://api.internal:8080/api", 1),
		"fragment in url":    strings.Replace(auroraConfig, "http://api.internal:8080", "http://api.internal:8080#x", 1),
		"non http scheme":    strings.Replace(auroraConfig, "http://api.internal:8080", "file:///etc/passwd", 1),
		"unknown nested key": strings.Replace(auroraConfig, `"aurora":{"server_url"`, `"aurora":{"image":"evil","server_url"`, 1),
		"nested null":        strings.Replace(auroraConfig, `,"aurora":{"server_url":"http://api.internal:8080"}`, `,"aurora":null`, 1),
		"server url type":    strings.Replace(auroraConfig, `"server_url":"http://api.internal:8080"`, `"server_url":7`, 1),
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

func TestValidEnrollmentToken(t *testing.T) {
	valid := "mse_" + strings.Repeat("a", 40)
	if !ValidEnrollmentToken(valid) {
		t.Fatal("server-issued enrollment token must be accepted")
	}
	for name, token := range map[string]string{
		"empty":        "",
		"wrong prefix": "msx_" + strings.Repeat("a", 40),
		"short":        "mse_" + strings.Repeat("a", 39),
		"long":         "mse_" + strings.Repeat("a", 41),
		"uppercase":    "mse_" + strings.Repeat("A", 40),
		"non hex":      "mse_" + strings.Repeat("z", 40),
		"node token":   "mcn_" + strings.Repeat("a", 43),
	} {
		t.Run(name, func(t *testing.T) {
			if ValidEnrollmentToken(token) {
				t.Fatalf("token %q must be rejected", token)
			}
		})
	}
}
