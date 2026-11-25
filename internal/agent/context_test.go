package agent

import (
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func TestContextThresholds(t *testing.T) {
	if m, c := ContextThresholds(true, 0, 0); m != 40 || c != 70 {
		t.Fatalf("small defaults = %d/%d", m, c)
	}
	if m, c := ContextThresholds(false, 0, 0); m != 60 || c != 80 {
		t.Fatalf("default = %d/%d", m, c)
	}
	if m, c := ContextThresholds(false, 90, 50); m > c {
		t.Fatalf("mask must not exceed compact: %d/%d", m, c)
	}
}

func toolRounds(n int) []api.Message {
	body := strings.Repeat("output line\n", 400)
	var h []api.Message
	for range n {
		h = append(h, api.Message{Role: "assistant", ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "read_file"}}}},
			api.Message{Role: "tool", Content: tools.EncodeToolSuccess("read_file", body)})
	}
	return h
}

func TestMaskForRequestOnlyUnderPressure(t *testing.T) {
	msgs := toolRounds(KeepIntactToolResults + 4)
	sent, b := maskForRequest(msgs, 1_000_000, false, 0)
	if b != 0 || &sent[0] != &msgs[0] {
		t.Fatal("no pressure, no masking, no copy")
	}
	sent, b = maskForRequest(msgs, 4000, false, 0)
	if b == 0 {
		t.Fatal("pressure past the threshold must mask")
	}
	stubbed, intact := 0, 0
	for _, m := range sent {
		if m.Role != "tool" {
			continue
		}
		if strings.Contains(m.Content, "elided") {
			stubbed++
		} else {
			intact++
		}
	}
	if stubbed != 4 || intact != KeepIntactToolResults {
		t.Fatalf("stubbed=%d intact=%d", stubbed, intact)
	}
	if strings.Contains(msgs[1].Content, "elided") {
		t.Fatal("the run's own record must keep every byte")
	}
}
