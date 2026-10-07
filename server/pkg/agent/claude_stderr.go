package agent

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
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

// claudeRegistryDiagnosticPendingCap bounds the bytes retained while deciding
// whether the current stderr line is the cosmetic diagnostic. The marker is
// fixed and short, so a prefix at this size cannot be it and is streamed
// through instead of held. Mirrors the sibling agentStderrTailBytes bound.
const claudeRegistryDiagnosticPendingCap = agentStderrTailBytes

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
// a Claude Code stderr stream while forwarding every other byte unchanged.
//
// It retains only the undecided prefix of the current line, bounded by
// claudeRegistryDiagnosticPendingCap. As soon as that prefix can no longer grow
// into the marker the buffered bytes are forwarded immediately, so a
// newline-less (or CR-only) stderr stream is streamed through rather than
// accumulated for the whole run. Flush releases a final partial marker prefix.
type claudeRegistryDiagnosticFilter struct {
	inner io.Writer

	mu sync.Mutex
	// pending holds the undecided prefix of the current line. trimStart is the
	// number of its leading bytes already verified as whitespace, so the
	// leading-whitespace scan never rescans what it has consumed.
	pending   []byte
	trimStart int
	state     claudeRegistryFilterState
}

type claudeRegistryFilterState int

const (
	// claudeRegistryDetect: the current line may still be the marker; pending
	// holds its undecided prefix.
	claudeRegistryDetect claudeRegistryFilterState = iota
	// claudeRegistryForward: the current line is known not to be the marker;
	// its bytes stream to inner through the next newline.
	claudeRegistryForward
	// claudeRegistryDrop: the current line is the marker; its bytes are
	// discarded through the next newline.
	claudeRegistryDrop
)

func newClaudeRegistryDiagnosticFilter(inner io.Writer) *claudeRegistryDiagnosticFilter {
	return &claudeRegistryDiagnosticFilter{inner: inner}
}

// Write filters p and reports the number of input bytes it consumed. On a write
// error it returns the bytes consumed before the failure rather than 0, so a
// partially forwarded stream is accounted for consistently.
func (f *claudeRegistryDiagnosticFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	consumed := 0
	for len(p) > 0 {
		switch f.state {
		case claudeRegistryForward:
			idx := bytes.IndexByte(p, '\n')
			if idx < 0 {
				if err := f.writeAll(p); err != nil {
					return consumed, err
				}
				consumed += len(p)
				p = nil
				continue
			}
			if err := f.writeAll(p[:idx+1]); err != nil {
				return consumed, err
			}
			consumed += idx + 1
			p = p[idx+1:]
			f.state = claudeRegistryDetect

		case claudeRegistryDrop:
			idx := bytes.IndexByte(p, '\n')
			if idx < 0 {
				consumed += len(p)
				p = nil
				continue
			}
			consumed += idx + 1
			p = p[idx+1:]
			f.state = claudeRegistryDetect

		default: // claudeRegistryDetect
			b := p[0]
			p = p[1:]
			consumed++
			if b == '\n' {
				if err := f.endLine(); err != nil {
					return consumed - 1, err
				}
				continue
			}
			f.pending = append(f.pending, b)
			matches, possible := f.classifyPending()
			switch {
			case matches:
				f.clearPending()
				f.state = claudeRegistryDrop
			case !possible || len(f.pending) >= claudeRegistryDiagnosticPendingCap:
				// The line can no longer become the diagnostic (or it has
				// outgrown the bound), so stop retaining it.
				if err := f.writeAll(f.pending); err != nil {
					f.clearPending()
					f.state = claudeRegistryForward
					return consumed, err
				}
				f.clearPending()
				f.state = claudeRegistryForward
			}
		}
	}
	return consumed, nil
}

// endLine classifies and releases the current line, which ended at a newline.
func (f *claudeRegistryDiagnosticFilter) endLine() error {
	if isClaudeRegistryDiagnostic(f.pending) {
		f.clearPending()
		return nil
	}
	f.pending = append(f.pending, '\n')
	err := f.writeAll(f.pending)
	f.clearPending()
	return err
}

// Flush releases the undecided prefix of a final unterminated line. A partial
// marker prefix is not the diagnostic, so it is forwarded unchanged.
func (f *claudeRegistryDiagnosticFilter) Flush() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.state {
	case claudeRegistryDrop:
		f.clearPending()
		return nil
	case claudeRegistryForward:
		return nil
	default:
		if len(f.pending) == 0 {
			return nil
		}
		if isClaudeRegistryDiagnostic(f.pending) {
			f.clearPending()
			return nil
		}
		err := f.writeAll(f.pending)
		f.clearPending()
		return err
	}
}

// classifyPending reports whether the undecided prefix of the current line is
// already the diagnostic (matches) or can still grow into it (possible). The
// leading-whitespace scan advances trimStart so each pending byte is examined
// at most once, and it holds while a leading multi-byte rune is still
// incomplete so a split whitespace rune is trimmed exactly like
// strings.TrimSpace.
func (f *claudeRegistryDiagnosticFilter) classifyPending() (matches, possible bool) {
	marker := claudeUnrecognizedModelMarker
	i := f.trimStart
	for i < len(f.pending) {
		if !utf8.FullRune(f.pending[i:]) {
			f.trimStart = i
			return false, true
		}
		r, size := utf8.DecodeRune(f.pending[i:])
		if !unicode.IsSpace(r) {
			break
		}
		i += size
	}
	f.trimStart = i
	trimmed := f.pending[i:]
	if len(trimmed) >= len(marker) {
		return strings.HasPrefix(string(trimmed), marker), false
	}
	return false, strings.HasPrefix(marker, string(trimmed))
}

func (f *claudeRegistryDiagnosticFilter) clearPending() {
	f.pending = f.pending[:0]
	f.trimStart = 0
}

// writeAll forwards b, handling short writes, so the number of bytes delivered
// to the inner writer is unambiguous before an error is reported.
func (f *claudeRegistryDiagnosticFilter) writeAll(b []byte) error {
	for len(b) > 0 {
		n, err := f.inner.Write(b)
		if n > 0 {
			b = b[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// isClaudeRegistryDiagnostic reports whether one stderr line is the cosmetic
// unrecognized-model registry diagnostic rather than a provider error.
func isClaudeRegistryDiagnostic(line []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(line)), claudeUnrecognizedModelMarker)
}
