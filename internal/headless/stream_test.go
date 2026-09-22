package headless

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(buf)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line is not JSON: %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestStreamJSONEventsInOrder(t *testing.T) {
	var seen string
	host := &fakeChat{responses: []api.ChatResponse{
		{Message: api.Message{Content: "let me echo", ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}}}}},
		{Message: api.Message{Content: "all done"}},
	}}
	var buf bytes.Buffer
	stream := NewStreamWriter(&buf)
	stream.Start("m", "/work", []string{"echo"})
	res, err := Run(context.Background(), host, echoRegistry(&seen), "do it", Options{Model: "m", Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Result(NewReport(res, "m"), nil); err != nil {
		t.Fatal(err)
	}
	lines := decodeLines(t, &buf)
	var types []string
	for _, l := range lines {
		types = append(types, l["type"].(string))
	}
	want := "start,text,tool_call,tool_result,text,result"
	if got := strings.Join(types, ","); got != want {
		t.Fatalf("event order = %s, want %s", got, want)
	}
	call, result := lines[2], lines[3]
	if call["id"] != result["id"] || call["name"] != "echo" {
		t.Fatalf("tool_call/tool_result ids should pair: %v / %v", call, result)
	}
	if args, _ := call["args"].(map[string]any); args["text"] != "hi" {
		t.Fatalf("args should be embedded as JSON, got %v", call["args"])
	}
	if result["ok"] != true || !strings.Contains(result["summary"].(string), "echoed: hi") {
		t.Fatalf("unexpected tool_result: %v", result)
	}
	final := lines[5]
	if final["output"] != "all done" || final["steps"].(float64) != 1 {
		t.Fatalf("result line should carry the report: %v", final)
	}
}

func TestStreamWriterMalformedArgsStayValidJSON(t *testing.T) {
	var buf bytes.Buffer
	s := NewStreamWriter(&buf)
	s.Assistant("", []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "x", Arguments: json.RawMessage(`{"a":`)}}})
	s.ToolResult(tools.ToolCall{Function: tools.ToolCallFunction{Name: "x"}}, strings.Repeat("é", 3000), true)
	lines := decodeLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	if lines[0]["args"] != `{"a":` {
		t.Fatalf("malformed args should be quoted as a string, got %v", lines[0]["args"])
	}
	sum := lines[1]["summary"].(string)
	if len(sum) > maxStreamSummary+len("…[truncated]") || !strings.HasSuffix(sum, "…[truncated]") {
		t.Fatalf("summary not truncated cleanly: len %d", len(sum))
	}
	if lines[1]["ok"] != false {
		t.Fatalf("failed result should have ok:false")
	}
}

func TestStreamResultCarriesError(t *testing.T) {
	var buf bytes.Buffer
	s := NewStreamWriter(&buf)
	_ = s.Result(Report{Model: "m"}, errors.New("boom"))
	lines := decodeLines(t, &buf)
	if lines[0]["type"] != "result" || lines[0]["error"] != "boom" {
		t.Fatalf("unexpected result line: %v", lines[0])
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) { f.n++; return 0, errors.New("closed") }

func TestStreamWriterErrorsAreSticky(t *testing.T) {
	fw := &failWriter{}
	s := NewStreamWriter(fw)
	s.Start("m", "", nil)
	s.Assistant("hi", nil)
	if err := s.Result(Report{}, nil); err == nil {
		t.Fatal("write error should surface from Result")
	}
	if fw.n != 1 {
		t.Fatalf("writer should stop after the first failure, wrote %d times", fw.n)
	}
}

func TestAugmentResultReachesModel(t *testing.T) {
	var seen string
	host := &fakeChat{responses: []api.ChatResponse{
		{Message: api.Message{ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}}}}},
		{Message: api.Message{Content: "done"}},
	}}
	_, err := Run(context.Background(), host, echoRegistry(&seen), "go", Options{Model: "m",
		AugmentResult: func(_ tools.ToolCall, r string) string { return r + "\n[extra rules]" }})
	if err != nil {
		t.Fatal(err)
	}
	msgs := host.requests[1].Messages
	last := msgs[len(msgs)-1]
	if last.Role != "tool" || !strings.HasSuffix(last.Content, "[extra rules]") {
		t.Fatalf("augmented result not sent to the model: %+v", last)
	}
}
