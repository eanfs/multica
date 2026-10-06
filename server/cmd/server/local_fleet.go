package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/redis/go-redis/v9"
)

type LocalFleetConfig struct {
	URL     string
	Secret  []byte
	Enabled bool
}

func resolveLocalFleet(cloudURL, localURL, secretFile string) (LocalFleetConfig, error) {
	if strings.TrimSpace(cloudURL) != "" && strings.TrimSpace(localURL) != "" {
		return LocalFleetConfig{}, errors.New("local Fleet and SaaS Cloud are mutually exclusive")
	}
	if strings.TrimSpace(localURL) == "" {
		return LocalFleetConfig{}, nil
	}
	u, err := url.ParseRequestURI(strings.TrimSpace(localURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return LocalFleetConfig{}, errors.New("invalid local Fleet URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return LocalFleetConfig{}, errors.New("local Fleet URL must use loopback")
	}
	secretFile = strings.TrimSpace(secretFile)
	if !filepath.IsAbs(secretFile) {
		return LocalFleetConfig{}, errors.New("Fleet service key requires an absolute file reference")
	}
	pathInfo, err := os.Lstat(secretFile)
	if err != nil || !pathInfo.Mode().IsRegular() {
		return LocalFleetConfig{}, errors.New("Fleet service key requires a regular file reference")
	}
	f, err := os.Open(secretFile)
	if err != nil {
		return LocalFleetConfig{}, errors.New("cannot open Fleet service key file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(pathInfo, info) || info.Mode().Perm()&0077 != 0 || info.Mode().Perm()&0400 == 0 {
		return LocalFleetConfig{}, errors.New("Fleet service key requires an owner-readable private regular file")
	}
	secret, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return LocalFleetConfig{}, errors.New("cannot read Fleet service key file")
	}
	if len(secret) > 65536 {
		return LocalFleetConfig{}, errors.New("Fleet service key file must not exceed 65536 bytes")
	}
	secret = bytes.TrimSpace(secret)
	if len(secret) < 32 || bytes.ContainsAny(secret, "\r\n") {
		return LocalFleetConfig{}, errors.New("Fleet service key must contain at least 32 bytes without line breaks")
	}
	return LocalFleetConfig{URL: strings.TrimRight(u.String(), "/"), Secret: secret, Enabled: true}, nil
}

func fleetPATVerifier(cloudURL string, local LocalFleetConfig, rdb redis.UniversalClient) *auth.CloudPATVerifier {
	cfg := auth.CloudPATVerifierConfig{FleetBaseURL: cloudURL, Redis: rdb}
	if local.Enabled {
		cfg.FleetBaseURL = local.URL
		cfg.Redis = nil
		cfg.ServiceSecret = local.Secret
	}
	return auth.NewCloudPATVerifier(cfg)
}
