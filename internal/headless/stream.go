package headless

import (
	"encoding/json"
	"io"
	"strconv"
	"sync"

	"github.com/javanhut/ollama_code/tools"
)

// Output formats accepted by --output-format.
const (
	FormatText       = "text"
	FormatJSON       = "json"
	FormatStreamJSON = "stream-json"
)

// maxStreamSummary bounds a tool_result event's summary. The full result went
// to the model; a consumer that needs it has --debug's trace.
const maxStreamSummary = 2048

// StreamEvent is one line of --output-format stream-json. Type is one of
// start, text, tool_call, tool_result. The terminal result line is a
// StreamResult instead.
type StreamEvent struct {
	Type    string          `json:"type"`
	Model   string          `json:"model,omitempty"`
	Cwd     string          `json:"cwd,omitempty"`
	Tools   []string        `json:"tools,omitempty"`
	Step    int             `json:"step,omitempty"`
	Text    string          `json:"text,omitempty"`
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Summary string          `json:"summary,omitempty"`
}

// StreamResult is the final stream-json line: the -json Report plus
// type:"result" (and an error message when the run failed).
type StreamResult struct {
	Type  string `json:"type"`
	Error string `json:"error,omitempty"`
	Report
}

// StreamWriter emits newline-delimited JSON events. Every line is written with
// a single Write so a reader never sees a partial object, and write errors are
// sticky: once the consumer has gone away the run keeps going silently.
type StreamWriter struct {
	mu      sync.Mutex
	w       io.Writer
	step    int
	calls   int
	results int
	err     error
}

// NewStreamWriter wraps w. w should be unbuffered (os.Stdout) or flushed by
// the caller; events are written as soon as they happen.
func NewStreamWriter(w io.Writer) *StreamWriter { return &StreamWriter{w: w} }

func (s *StreamWriter) emit(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	line, err := json.Marshal(v)
	if err != nil {
		s.err = err
		return
	}
	line = append(line, '\n')
	_, s.err = s.w.Write(line)
	if f, ok := s.w.(interface{ Sync() error }); ok && s.err == nil {
		_ = f.Sync() // best effort: a pipe's Sync fails harmlessly
	}
}

// Start emits the opening event.
func (s *StreamWriter) Start(model, cwd string, toolNames []string) {
	s.emit(StreamEvent{Type: "start", Model: model, Cwd: cwd, Tools: toolNames})
}

// Assistant emits the model's prose (if any) and one tool_call per call. It
// matches agent.Options.OnAssistant.
func (s *StreamWriter) Assistant(content string, calls []tools.ToolCall) {
	s.mu.Lock()
	s.step++
	step := s.step
	s.mu.Unlock()
	if content != "" {
		s.emit(StreamEvent{Type: "text", Step: step, Text: content})
	}
	for _, c := range calls {
		s.mu.Lock()
		s.calls++
		id := "call_" + strconv.Itoa(s.calls)
		s.mu.Unlock()
		args := c.Function.Arguments
		if !json.Valid(args) {
			// A malformed argument blob must not break the NDJSON line.
			quoted, _ := json.Marshal(string(args))
			args = quoted
		}
		s.emit(StreamEvent{Type: "tool_call", Step: step, ID: id, Name: c.Function.Name, Args: args})
	}
}

// ToolResult emits one tool_result. It matches agent.Options.OnToolResult.
// Results arrive in the order the calls were emitted, so the id is recovered
// from a counter rather than threaded through the loop.
func (s *StreamWriter) ToolResult(call tools.ToolCall, result string, failed bool) {
	s.mu.Lock()
	s.results++
	id := "call_" + strconv.Itoa(s.results)
	step := s.step
	s.mu.Unlock()
	ok := !failed
	s.emit(StreamEvent{Type: "tool_result", Step: step, ID: id, Name: call.Function.Name, OK: &ok, Summary: truncateSummary(result)})
}

// Result emits the terminal event. runErr may be nil.
func (s *StreamWriter) Result(r Report, runErr error) error {
	ev := StreamResult{Type: "result", Report: r}
	if runErr != nil {
		ev.Error = runErr.Error()
	}
	s.emit(ev)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func truncateSummary(s string) string {
	if len(s) <= maxStreamSummary {
		return s
	}
	cut := maxStreamSummary
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…[truncated]"
}
