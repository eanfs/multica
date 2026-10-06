package agent

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// claudeUnrecognizedModelMarker is the prefix of Claude Code's purely
// informational model-registry diagnostic. Claude Code emits it once per run
// for any model id outside its built-in catalog, including a valid
// operator-configured gateway model such as Volcengine ARK Agent Plan's
// "glm-5.3-flash[1M]". It canonicalizes the id to lowercase for its own
// registry before printing it, while the request that goes on the wire keeps
// the configured base name with its original case and a trailing "[1M]"
// context marker moved into the "context-1m" beta header. The line therefore
// says nothing about whether the provider accepts the request, and a real
// provider error still arrives through the normal result path. Verified against
// Claude Code 2.1.289.
const claudeUnrecognizedModelMarker = "[claude-code:unrecognized_model]"

// claudeStderrSink wraps the Claude stderr writer with the registry-diagnostic
// filter, but only for an operator-configured custom endpoint
// (ANTHROPIC_BASE_URL), where a non-Anthropic model id is expected and the
// diagnostic is pure noise. A first-party run keeps it: there an unrecognized
// model is a real misconfiguration worth surfacing.
func claudeStderrSink(env map[string]string, inner io.Writer) (io.Writer, *claudeRegistryDiagnosticFilter) {
	if strings.TrimSpace(env["ANTHROPIC_BASE_URL"]) == "" {
		return inner, nil
	}
	filter := newClaudeRegistryDiagnosticFilter(inner)
	return filter, filter
}

// claudeRegistryDiagnosticFilter drops claudeUnrecognizedModelMarker lines from
// a Claude Code stderr stream while forwarding every other byte unchanged. It
// buffers until a newline so a diagnostic split across writes is still
// recognized; Flush releases a final unterminated line.
type claudeRegistryDiagnosticFilter struct {
	inner io.Writer

	mu      sync.Mutex
	pending []byte
}

func newClaudeRegistryDiagnosticFilter(inner io.Writer) *claudeRegistryDiagnosticFilter {
	return &claudeRegistryDiagnosticFilter{inner: inner}
}

func (f *claudeRegistryDiagnosticFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, p...)
	for {
		idx := bytes.IndexByte(f.pending, '\n')
		if idx < 0 {
			break
		}
		if err := f.emit(f.pending[:idx+1]); err != nil {
			return 0, err
		}
		f.pending = append(f.pending[:0], f.pending[idx+1:]...)
	}
	return len(p), nil
}

// Flush forwards any final unterminated line.
func (f *claudeRegistryDiagnosticFilter) Flush() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil
	}
	line := f.pending
	f.pending = nil
	return f.emit(line)
}

func (f *claudeRegistryDiagnosticFilter) emit(line []byte) error {
	if isClaudeRegistryDiagnostic(line) {
		return nil
	}
	_, err := f.inner.Write(line)
	return err
}

// isClaudeRegistryDiagnostic reports whether one stderr line is the cosmetic
// unrecognized-model registry diagnostic rather than a provider error.
func isClaudeRegistryDiagnostic(line []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(line)), claudeUnrecognizedModelMarker)
}
