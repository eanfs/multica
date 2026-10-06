package model

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// The managed Aurora sandbox runtime reads exactly one single-use enrollment
// secret from a fixed read-only path. The Fleet writes that file into the node
// secrets volume and mounts the volume read-only at AuroraEnrollmentDir, so the
// path is part of the fixed layout contract, never caller input.
const (
	AuroraEnrollmentDir     = "/secrets"
	AuroraEnrollmentFile    = AuroraEnrollmentDir + "/aurora-enrollment"
	AuroraManagedEnv        = "MULTICA_MANAGED"
	AuroraServerURLEnv      = "MULTICA_SERVER_URL"
	AuroraEnrollmentFileEnv = "MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE"
)

// enrollmentTokenPattern matches the server-issued managed enrollment secret:
// "mse_" plus 40 lowercase hex characters. Validation is local so junk secrets
// never reach the managed daemon or the enrollment endpoint.
var enrollmentTokenPattern = regexp.MustCompile(`^mse_[0-9a-f]{40}$`)

// AuroraConfig selects the managed-sandbox execution profile for one Fleet
// deployment. It is administrator-owned public configuration: the sandbox image
// is Config.Image and model credentials belong to the sandbox's own provider
// secret mounts, never here. When nil the deployment runs the default
// Claude-only node profile.
type AuroraConfig struct {
	// ServerURL is the container-reachable Multica API origin the managed
	// daemon enrolls against and calls back to. It is not a client-supplied
	// image, path or credential.
	ServerURL string `json:"server_url"`
}

// Validate rejects an Aurora profile that could not enroll: the fixed layout
// only supports a plain http(s) origin with no user info, query or fragment.
func (a AuroraConfig) Validate() error {
	if !validOrigin(a.ServerURL) {
		return fmt.Errorf("%w: aurora server_url must be an http(s) origin", ErrInvalidRequest)
	}
	return nil
}

// ValidEnrollmentToken reports whether token matches the server-issued managed
// enrollment format. The Fleet never derives identity from the secret.
func ValidEnrollmentToken(token string) bool {
	return enrollmentTokenPattern.MatchString(token)
}

// validOrigin accepts only a bare http(s) origin, so a configured server URL
// cannot smuggle credentials, a path, a query or a fragment into the managed
// daemon environment.
func validOrigin(raw string) bool {
	if strings.TrimSpace(raw) != raw || raw == "" || strings.Contains(raw, "#") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Path == "" && u.RawPath == ""
}
