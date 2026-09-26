package aurorafleet

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const (
	// maxTokenFileBytes caps the token file on disk, encoded form included.
	maxTokenFileBytes = 256
	// minTokenBytes is the minimum amount of entropy a control token must
	// carry (32 random bytes) in either supported encoding.
	minTokenBytes = 32
	// maxTokenBytes is the largest raw token accepted from either encoding,
	// bounding the comparison buffer.
	maxTokenBytes = 64
)

// ControlAuth holds the fleet control bearer token in memory and guards every
// internal route with it. The token is read once, at startup, from a file the
// operator provisions with restrictive permissions; it is never accepted from
// the environment and never logged.
type ControlAuth struct {
	token []byte
}

// LoadControlAuth reads and validates the fleet control token file. The file
// must be a regular file (not a symlink), have mode 0400 or 0600, be at most
// 256 bytes, and contain at least 32 random bytes encoded as base64url or
// hex. Only the decoded token bytes are kept in memory.
func LoadControlAuth(path string) (*ControlAuth, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read control token file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("control token file must be a regular file, not a symlink")
	}
	if perm := info.Mode().Perm(); perm != 0o400 && perm != 0o600 {
		return nil, fmt.Errorf("control token file mode %o, want 0400 or 0600", perm)
	}
	if info.Size() > maxTokenFileBytes {
		return nil, fmt.Errorf("control token file is %d bytes, want at most %d", info.Size(), maxTokenFileBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read control token file: %w", err)
	}
	token, err := decodeControlToken(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, err
	}
	return &ControlAuth{token: token}, nil
}

// decodeControlToken decodes the trimmed file content as base64url or hex and
// enforces the minimum entropy bound. Hex is tried first for even-length
// strings: every hex digit set is also a valid base64url alphabet subset, so
// the base64 decoder would otherwise silently misread a hex token.
func decodeControlToken(encoded string) ([]byte, error) {
	decode := func() ([]byte, bool) {
		if len(encoded)%2 == 0 {
			if b, err := hex.DecodeString(encoded); err == nil {
				return b, true
			}
		}
		b, err := base64.RawURLEncoding.DecodeString(encoded)
		return b, err == nil
	}
	b, ok := decode()
	if !ok {
		return nil, errors.New("control token is neither base64url nor hex")
	}
	if len(b) < minTokenBytes || len(b) > maxTokenBytes {
		return nil, fmt.Errorf("control token carries %d bytes, want at least %d and at most %d", len(b), minTokenBytes, maxTokenBytes)
	}
	return b, nil
}

// Middleware returns the bearer gate for the controller's routes. Missing,
// malformed, and wrong tokens receive the identical 401 response so the
// endpoint leaks nothing about the configured credential.
func (a *ControlAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r.Header.Get("Authorization")) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorized compares the presented bearer bytes with the configured token in
// constant time.
func (a *ControlAuth) authorized(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	presented := header[len(prefix):]
	if len(presented) == 0 || len(presented) > 2*maxTokenBytes {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), a.token) == 1
}
