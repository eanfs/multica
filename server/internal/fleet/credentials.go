package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

var (
	// ErrProfileMissing means the owner has no explicit credential profile.
	// There is deliberately no global default profile.
	ErrProfileMissing = errors.New("claude credential profile missing")
	// ErrProfileInvalid means the owner maps to a profile that does not exist.
	ErrProfileInvalid = errors.New("claude credential profile invalid")
)

// Profile is the environment handed to a node's model process.
type Profile struct {
	Env map[string]string `json:"env"`
}

// credentialsFile is the on-disk shape. Model secrets only ever live here, never
// in the database or logs.
type credentialsFile struct {
	Users    map[string]string  `json:"users"`
	Profiles map[string]Profile `json:"profiles"`
}

// Credentials is the loaded owner-to-profile mapping.
type Credentials struct {
	users    map[string]string
	profiles map[string]Profile
}

// allowedModelEnv is the only environment a node entrypoint may export into the
// model process. Everything else (MULTICA_*, PATH, HOME) is rejected so a
// credentials file cannot smuggle platform credentials into a node.
var allowedModelEnv = map[string]struct{}{
	"ANTHROPIC_API_KEY":       {},
	"ANTHROPIC_AUTH_TOKEN":    {},
	"ANTHROPIC_BASE_URL":      {},
	"ANTHROPIC_MODEL":         {},
	"CLAUDE_CODE_USE_BEDROCK": {},
	"API_TIMEOUT_MS":          {},
}

// maxCredentialValueLen bounds a single env value so a credentials file cannot
// overshoot the process environment limit or hide an injected payload.
const maxCredentialValueLen = 8192

// LoadCredentials reads and validates the private credentials file at path.
// Validation happens once at load time so a bad file fails startup instead of
// one owner's node launch.
func LoadCredentials(path string) (*Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fleet credentials: %w", err)
	}
	var file credentialsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse fleet credentials: %w", err)
	}
	for name, profile := range file.Profiles {
		if err := validateProfileEnv(name, profile.Env); err != nil {
			return nil, err
		}
	}
	return &Credentials{users: file.Users, profiles: file.Profiles}, nil
}

// validateProfileEnv enforces the model env allowlist. Error messages name the
// profile and variable only; values are never echoed.
func validateProfileEnv(name string, env map[string]string) error {
	for key, value := range env {
		if _, ok := allowedModelEnv[key]; !ok {
			return fmt.Errorf("credential profile %q: env %q is not an allowed model variable", name, key)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("credential profile %q: env %q contains a newline or NUL", name, key)
		}
		if len(value) > maxCredentialValueLen {
			return fmt.Errorf("credential profile %q: env %q exceeds %d bytes", name, key, maxCredentialValueLen)
		}
	}
	return nil
}

// Resolve returns a copy of the profile mapped to ownerID. An unmapped owner is
// ErrProfileMissing; a mapping to an unknown profile is ErrProfileInvalid.
func (c *Credentials) Resolve(ownerID string) (Profile, error) {
	name, ok := c.users[ownerID]
	if !ok || name == "" {
		return Profile{}, ErrProfileMissing
	}
	profile, ok := c.profiles[name]
	if !ok {
		return Profile{}, ErrProfileInvalid
	}
	// Return a copy: callers may decorate the env for one node, and owners
	// sharing a profile must not observe that mutation.
	env := make(map[string]string, len(profile.Env))
	for key, value := range profile.Env {
		env[key] = value
	}
	return Profile{Env: env}, nil
}
