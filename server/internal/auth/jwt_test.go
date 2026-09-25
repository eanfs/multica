package auth

import (
	"strings"
	"testing"
)

// TestGenerateManagedEnrollmentTokenFormat pins the managed-enrollment secret
// contract: the "mse_" prefix, 40 lowercase hex characters from 20 random
// bytes, and entropy good enough that a batch of draws never repeats.
func TestGenerateManagedEnrollmentTokenFormat(t *testing.T) {
	const draws = 128
	seen := make(map[string]struct{}, draws)
	for i := 0; i < draws; i++ {
		token, err := GenerateManagedEnrollmentToken()
		if err != nil {
			t.Fatalf("generate managed enrollment token: %v", err)
		}
		if !strings.HasPrefix(token, "mse_") {
			t.Fatalf("token %q does not carry the mse_ prefix", token)
		}
		payload := strings.TrimPrefix(token, "mse_")
		if len(payload) != 40 {
			t.Fatalf("token payload %q is %d chars, want 40", payload, len(payload))
		}
		for _, c := range payload {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("token payload %q contains non-lowercase-hex %q", payload, c)
			}
		}
		if _, duplicate := seen[token]; duplicate {
			t.Fatalf("duplicate token after %d draws: %q", i, token)
		}
		seen[token] = struct{}{}
	}
}
