package tools

import (
	"strings"
	"testing"
)

func TestToolResultEnvelope(t *testing.T) {
	raw := EncodeToolSuccess("read_file", "main.go:12: hello")
	got, ok := DecodeToolResult(raw)
	if !ok || !got.OK || got.Summary != "read_file completed" || len(got.Evidence) != 1 {
		t.Fatalf("unexpected envelope: %#v (%s)", got, raw)
	}

	raw = EncodeToolFailure("read_file failed", "use a relative path", true)
	got, ok = DecodeToolResult(raw)
	if !ok || got.OK || !got.Retryable || got.Hint == "" {
		t.Fatalf("unexpected failure envelope: %#v", got)
	}
}

func TestToolResultTruncationKeepsTail(t *testing.T) {
	raw := EncodeToolSuccess("run_shell", strings.Repeat("a", defaultResultLimit)+"TAIL")
	got, ok := DecodeToolResult(raw)
	if !ok || !got.Truncated || !strings.Contains(got.Evidence[0], "TAIL") {
		t.Fatalf("expected truncated result retaining tail: %#v", got)
	}
}
