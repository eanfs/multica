package cloudruntime

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// LocalFleetConfig is the resolved local-Fleet wiring. The secret is kept in
// memory only: it comes from a private file and is never logged, returned, or
// copied into another Config value.
type LocalFleetConfig struct {
	LocalURL       string
	LocalSecret    string
	LocalEnabled   bool
	LocalNamespace string
}

// ResolveLocalFleetConfig reads the local Fleet settings and fails closed on
// every ambiguous or partial configuration:
//
//   - MULTICA_CLOUD_URL and MULTICA_LOCAL_FLEET_URL are mutually exclusive;
//   - a local URL requires both a secret file and a namespace;
//   - the local URL must be an absolute http(s) URL with no credentials,
//     query, or fragment;
//   - the secret file must be readable and non-blank.
//
// With no local URL (including a whitespace-only one) the zero config is
// returned and the cloud runtime proxy and mcn_ verifier keep their existing
// SaaS/self-hosted behaviour.
func ResolveLocalFleetConfig(getenv func(string) string) (LocalFleetConfig, error) {
	cloudURL := strings.TrimSpace(getenv("MULTICA_CLOUD_URL"))
	localURL := strings.TrimSpace(getenv("MULTICA_LOCAL_FLEET_URL"))
	secretFile := strings.TrimSpace(getenv("MULTICA_LOCAL_FLEET_SECRET_FILE"))
	namespace := strings.TrimSpace(getenv("MULTICA_LOCAL_FLEET_NAMESPACE"))

	if cloudURL != "" && localURL != "" {
		return LocalFleetConfig{}, fmt.Errorf("MULTICA_CLOUD_URL and MULTICA_LOCAL_FLEET_URL are mutually exclusive")
	}
	if localURL == "" {
		return LocalFleetConfig{}, nil
	}
	if secretFile == "" || namespace == "" {
		return LocalFleetConfig{}, fmt.Errorf("local fleet requires MULTICA_LOCAL_FLEET_SECRET_FILE and MULTICA_LOCAL_FLEET_NAMESPACE")
	}
	parsedURL, err := url.Parse(localURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" ||
		parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return LocalFleetConfig{}, fmt.Errorf("MULTICA_LOCAL_FLEET_URL must be an absolute http(s) URL without credentials, query, or fragment")
	}
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return LocalFleetConfig{}, fmt.Errorf("read local fleet secret: %w", err)
	}
	if strings.TrimSpace(string(secret)) == "" {
		return LocalFleetConfig{}, fmt.Errorf("local fleet secret is empty")
	}
	return LocalFleetConfig{
		LocalURL:       localURL,
		LocalSecret:    strings.TrimSpace(string(secret)),
		LocalEnabled:   true,
		LocalNamespace: namespace,
	}, nil
}
