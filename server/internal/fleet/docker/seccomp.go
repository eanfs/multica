package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// maxSeccompProfileBytes bounds the operator-owned seccomp profile. The profile
// is embedded inline in HostConfig.SecurityOpt, so an unbounded file would be an
// unbounded container create request.
const maxSeccompProfileBytes = 1 << 20

// resolveAuroraSeccomp returns the compact inline JSON of the configured Aurora
// seccomp profile. The Docker SDK sends SecurityOpt verbatim to dockerd, which
// parses the value after "seccomp=" as JSON, not as a host path; a path would
// make every Aurora node fail to start with "Decoding seccomp profile failed".
// The profile is therefore read and validated here and only its content reaches
// the wire. A nil profile returns an empty string so the Claude HostConfig is
// untouched. Every read/parse failure is surfaced, never swallowed, so a
// missing, unreadable or malformed operator profile fails closed instead of
// silently weakening the container.
func resolveAuroraSeccomp(aurora *model.AuroraConfig) (string, error) {
	if aurora == nil {
		return "", nil
	}
	return loadSeccompProfile(aurora.SeccompProfile)
}

// loadSeccompProfile reads, size-bounds and JSON-validates one operator-owned
// seccomp profile. It requires a regular file that decodes to a JSON object
// declaring a non-empty defaultAction; an empty profile would let the daemon
// fall back to a weaker default, so it is rejected here.
func loadSeccompProfile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: aurora seccomp profile is unreadable", model.ErrUnavailable)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%w: aurora seccomp profile is unreadable", model.ErrUnavailable)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: aurora seccomp profile must be a regular file", model.ErrInvalidRequest)
	}
	if info.Size() <= 0 || info.Size() > maxSeccompProfileBytes {
		return "", fmt.Errorf("%w: aurora seccomp profile size is out of bounds", model.ErrInvalidRequest)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSeccompProfileBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: aurora seccomp profile is unreadable", model.ErrUnavailable)
	}
	if len(raw) > maxSeccompProfileBytes {
		return "", fmt.Errorf("%w: aurora seccomp profile exceeds the size bound", model.ErrInvalidRequest)
	}
	var probe struct {
		DefaultAction string `json:"defaultAction"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", fmt.Errorf("%w: aurora seccomp profile is not a valid JSON object", model.ErrInvalidRequest)
	}
	if probe.DefaultAction == "" {
		return "", fmt.Errorf("%w: aurora seccomp profile must declare a defaultAction", model.ErrInvalidRequest)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "", fmt.Errorf("%w: aurora seccomp profile is not valid JSON", model.ErrInvalidRequest)
	}
	return compact.String(), nil
}
