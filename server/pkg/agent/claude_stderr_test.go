package agent

import (
	"bytes"
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
	// line until the newline rather than forward the first half.
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

func TestClaudeRegistryDiagnosticFilterFlushesFinalLine(t *testing.T) {
	var out bytes.Buffer
	filter := newClaudeRegistryDiagnosticFilter(&out)
	// No trailing newline: a real error must still reach the tail on Flush.
	if _, err := filter.Write([]byte("API Error: timeout")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("unterminated line forwarded before flush: %q", out.String())
	}
	if err := filter.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := out.String(); got != "API Error: timeout" {
		t.Fatalf("flushed stderr = %q", got)
	}
}
