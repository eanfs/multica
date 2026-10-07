package agent

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClaudeStderrSinkFiltersOnlyCustomEndpoint(t *testing.T) {
	// A first-party Claude run keeps the diagnostic: there an unrecognized
	// model id is a real misconfiguration.
	var plain bytes.Buffer
	sink, filter := claudeStderrSink(map[string]string{}, &plain)
	if filter != nil {
		t.Fatal("filter installed without a configured ANTHROPIC_BASE_URL")
	}
	diagnostic := claudeUnrecognizedModelMarker + " {\"model\":\"glm-5.3-flash[1m]\",\"query_source\":\"sdk\"}" + "\n"
	if _, err := sink.Write([]byte(diagnostic)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(plain.String(), claudeUnrecognizedModelMarker) {
		t.Fatalf("first-party stderr lost the diagnostic: %q", plain.String())
	}

	// A custom endpoint legitimately serves model ids Claude Code does not
	// know, so the diagnostic is dropped.
	var custom bytes.Buffer
	sink, filter = claudeStderrSink(map[string]string{"ANTHROPIC_BASE_URL": "https://ark.cn-beijing.volces.com/api/plan"}, &custom)
	if filter == nil {
		t.Fatal("filter not installed for a configured ANTHROPIC_BASE_URL")
	}
	if _, err := sink.Write([]byte(diagnostic)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if custom.Len() != 0 {
		t.Fatalf("custom endpoint stderr = %q, want the diagnostic dropped", custom.String())
	}
}

func TestClaudeRegistryDiagnosticFilterForwarding(t *testing.T) {
	marker := claudeUnrecognizedModelMarker
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "diagnostic dropped",
			input: marker + " {\"model\":\"glm-5.3-flash[1m]\"}" + "\n",
			want:  "",
		},
		{
			name:  "provider error kept",
			input: "API Error: 404 UnsupportedModel\n",
			want:  "API Error: 404 UnsupportedModel\n",
		},
		{
			name:  "diagnostic does not swallow the next line",
			input: marker + " {}\nAPI Error: 401\n",
			want:  "API Error: 401\n",
		},
		{
			name:  "marker mid-line is not the diagnostic",
			input: "note: saw " + marker + " once\n",
			want:  "note: saw " + marker + " once\n",
		},
		{
			name:  "empty line kept",
			input: "\n",
			want:  "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			filter := newClaudeRegistryDiagnosticFilter(&out)
			n, err := filter.Write([]byte(tc.input))
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			if n != len(tc.input) {
				t.Fatalf("Write returned %d, want %d (every byte consumed)", n, len(tc.input))
			}
			if err := filter.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("filtered stderr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClaudeRegistryDiagnosticFilterHandlesSplitWrites(t *testing.T) {
	var out bytes.Buffer
	filter := newClaudeRegistryDiagnosticFilter(&out)
	line := claudeUnrecognizedModelMarker + " {\"model\":\"glm-5.3-flash[1m]\"}" + "\n"
	split := len("[claude-code:unrecognized")
	// The marker straddles the two writes: the filter must hold the partial
	// line until it can decide rather than forward the first half.
	if _, err := filter.Write([]byte(line[:split])); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("partial line forwarded early: %q", out.String())
	}
	if _, err := filter.Write([]byte(line[split:])); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("split diagnostic not dropped: %q", out.String())
	}
}

func TestClaudeRegistryDiagnosticFilterForwardsUnterminatedLineLive(t *testing.T) {
	var out bytes.Buffer
	filter := newClaudeRegistryDiagnosticFilter(&out)
	const line = "API Error: timeout"
	n, err := filter.Write([]byte(line))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(line) {
		t.Fatalf("Write returned %d, want %d", n, len(line))
	}
	// The very first byte already rules out the marker, so the rest of the
	// line must stream through instead of waiting for a newline that may never
	// arrive on a newline-less stderr stream.
	if got := out.String(); got != line {
		t.Fatalf("unterminated line = %q, want %q (forwarded live)", got, line)
	}
	if err := filter.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := out.String(); got != line {
		t.Fatalf("flush duplicated or dropped bytes: %q", got)
	}
}

func TestClaudeRegistryDiagnosticFilterFlushesPartialMarkerPrefix(t *testing.T) {
	var out bytes.Buffer
	filter := newClaudeRegistryDiagnosticFilter(&out)
	prefix := claudeUnrecognizedModelMarker[:8]
	if _, err := filter.Write([]byte(prefix)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("partial marker prefix forwarded early: %q", out.String())
	}
	if err := filter.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := out.String(); got != prefix {
		t.Fatalf("flushed stderr = %q, want %q", got, prefix)
	}
}

func TestClaudeRegistryDiagnosticFilterForwardsLongUnterminatedStream(t *testing.T) {
	// All-whitespace is the worst case for an append-until-newline buffer: no
	// byte can rule out the marker, so the whole stream would otherwise be
	// retained for the entire run. The filter must cap the pending prefix and
	// stream the rest through.
	stream := bytes.Repeat([]byte{' '}, 1<<20)
	var out bytes.Buffer
	filter := newClaudeRegistryDiagnosticFilter(&out)
	n, err := filter.Write(stream)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(stream) {
		t.Fatalf("Write returned %d, want %d", n, len(stream))
	}
	if out.Len() != len(stream) {
		t.Fatalf("forwarded %d bytes, want the full %d (streamed, not retained)", out.Len(), len(stream))
	}
	filter.mu.Lock()
	pending := len(filter.pending)
	filter.mu.Unlock()
	if pending > claudeRegistryDiagnosticPendingCap {
		t.Fatalf("pending buffer = %d bytes, want <= %d (bounded)", pending, claudeRegistryDiagnosticPendingCap)
	}
	if err := filter.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if out.Len() != len(stream) {
		t.Fatalf("after flush forwarded %d bytes, want %d", out.Len(), len(stream))
	}
}

func TestClaudeRegistryDiagnosticFilterForwardsLongNonMarkerStream(t *testing.T) {
	// A newline-less stream that diverges from the marker immediately is also
	// forwarded live and never accumulates in the pending buffer.
	stream := bytes.Repeat([]byte("not the marker "), 1<<16)
	var out bytes.Buffer
	filter := newClaudeRegistryDiagnosticFilter(&out)
	if _, err := filter.Write(stream); err != nil {
		t.Fatalf("write: %v", err)
	}
	filter.mu.Lock()
	pending := len(filter.pending)
	filter.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending buffer = %d bytes, want 0 for a decided line", pending)
	}
	if out.Len() != len(stream) {
		t.Fatalf("forwarded %d bytes, want %d", out.Len(), len(stream))
	}
}

func TestClaudeRegistryDiagnosticFilterDropsLongMarkerLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		line []byte
	}{
		{
			name: "unterminated",
			line: append([]byte(claudeUnrecognizedModelMarker), bytes.Repeat([]byte{'x'}, 1<<16)...),
		},
		{
			name: "terminated",
			line: append(append([]byte(claudeUnrecognizedModelMarker), bytes.Repeat([]byte{'y'}, 1<<16)...), '\n'),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			filter := newClaudeRegistryDiagnosticFilter(&out)
			if _, err := filter.Write(tc.line); err != nil {
				t.Fatalf("write: %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("long diagnostic prefix forwarded: %d bytes", out.Len())
			}
			if err := filter.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("long diagnostic emitted on flush: %d bytes", out.Len())
			}
		})
	}
}

func TestClaudeRegistryDiagnosticFilterNewlineHeavyBursts(t *testing.T) {
	var in, want bytes.Buffer
	for i := 0; i < 5000; i++ {
		if i%500 == 0 {
			in.WriteString(claudeUnrecognizedModelMarker + fmt.Sprintf(" {\"n\":%d}\n", i))
		}
		line := fmt.Sprintf("line %04d\n", i)
		in.WriteString(line)
		want.WriteString(line)
	}
	var out bytes.Buffer
	filter := newClaudeRegistryDiagnosticFilter(&out)
	payload := in.Bytes()
	total := 0
	// Feed in small bursts so the drain path runs repeatedly.
	for start := 0; start < len(payload); start += 7 {
		end := start + 7
		if end > len(payload) {
			end = len(payload)
		}
		n, err := filter.Write(payload[start:end])
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if n != end-start {
			t.Fatalf("Write returned %d, want %d", n, end-start)
		}
		total += n
	}
	if err := filter.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if total != len(payload) {
		t.Fatalf("wrote %d bytes, want %d", total, len(payload))
	}
	if got := out.String(); got != want.String() {
		t.Fatalf("filtered %d bytes, want %d; first mismatch at %d", len(got), want.Len(), firstDiff(got, want.String()))
	}
}

func firstDiff(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

type failAfterWriter struct {
	buf    bytes.Buffer
	failAt int
	writes int
	err    error
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.writes >= w.failAt {
		return 0, w.err
	}
	w.writes++
	return w.buf.Write(p)
}

func TestClaudeRegistryDiagnosticFilterAccountsConsumedBytesOnFailure(t *testing.T) {
	boom := errors.New("inner write failed")
	inner := &failAfterWriter{failAt: 1, err: boom}
	filter := newClaudeRegistryDiagnosticFilter(inner)
	input := []byte("one\ntwo\n")
	n, err := filter.Write(input)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n <= 0 || n >= len(input) {
		t.Fatalf("Write returned %d on a failed write, want 0 < n < %d", n, len(input))
	}
	if n != inner.buf.Len() {
		t.Fatalf("Write returned %d but forwarded %d bytes to the inner writer", n, inner.buf.Len())
	}
}
