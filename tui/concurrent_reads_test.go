package tui

import (
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// A batch of pure reads starts every call at once even on a small model,
// whose parallel limit is otherwise 1; a batch with a write stays serial.
func TestPureReadBatchRunsConcurrently(t *testing.T) {
	cases := []struct {
		name  string
		calls []tools.ToolCall
		want  int
	}{
		{"reads", []tools.ToolCall{
			tc("read_file", `{"path":"a"}`), tc("grep", `{"pattern":"x"}`), tc("list_directory", `{"path":"."}`),
		}, 3},
		{"mixed", []tools.ToolCall{
			tc("read_file", `{"path":"a"}`), tc("write_file", `{"path":"b","content":"x"}`),
		}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := subagentTestModel()
			m.mode = AutoMode
			m.profile = ModelProfile{ParamsB: 3} // small: limit 1
			if m.parallelToolLimit() != 1 {
				t.Fatalf("precondition: small-model limit = %d", m.parallelToolLimit())
			}
			m.pending = &pendingBatch{gen: 1, calls: c.calls,
				results: make([]api.Message, len(c.calls)), started: make([]bool, len(c.calls))}
			m.processPendingTools()
			started := 0
			for _, s := range m.pending.started {
				if s {
					started++
				}
			}
			if started != c.want {
				t.Fatalf("started %d calls, want %d", started, c.want)
			}
		})
	}
}
