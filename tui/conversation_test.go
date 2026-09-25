package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// captureHost records the chat requests ocode sends.
type captureHost struct {
	mu   sync.Mutex
	reqs []api.ChatRequest
}

func (c *captureHost) serve(t *testing.T) string {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		c.mu.Lock()
		c.reqs = append(c.reqs, req)
		c.mu.Unlock()
		w.Write([]byte(`{"done":true,"message":{"role":"assistant","content":"ok"}}` + "\n"))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func (c *captureHost) last(t *testing.T) api.ChatRequest {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		t.Fatal("no request reached the host")
	}
	return c.reqs[len(c.reqs)-1]
}

func conversationModel(t *testing.T, host *captureHost) *Model {
	m := interruptTestModel()
	m.profile = ModelProfile{SupportsTools: true}
	m.input = textarea.New()
	m.host.SetURI(host.serve(t))
	m.modelName = "test-model"
	return m
}

func (c *captureHost) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// send submits text and waits for its request to reach the host: the chat
// request goes out on its own goroutine as soon as the stream starts.
func send(t *testing.T, m *Model, host *captureHost, text string) {
	t.Helper()
	before := host.count()
	m.input.SetValue(text)
	if cmd := m.submit(); cmd == nil {
		t.Fatalf("%q started no turn", text)
	}
	deadline := time.Now().Add(5 * time.Second)
	for host.count() == before {
		if time.Now().After(deadline) {
			t.Fatalf("%q: no request reached the host", text)
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.abandonTurnWork()
}

// "hello" gets a plain chat reply: no tools offered, and an instruction to
// greet and ask in text. The session in the log got a picker instead.
func TestGreetingIsAnsweredInChat(t *testing.T) {
	host := &captureHost{}
	m := conversationModel(t, host)
	send(t, m, host, "hello")

	req := host.last(t)
	if len(req.Tools) != 0 {
		t.Fatalf("greeting request offered tools: %v", len(req.Tools))
	}
	tail := req.Messages[len(req.Messages)-1].Content
	if !strings.Contains(tail, "Reply in plain text") || strings.Contains(tail, "AVAILABLE TOOLS") {
		t.Fatalf("tail does not ask for a plain-text reply:\n%s", tail)
	}
}

// The user's next message states the task, and that turn has the full toolset.
func TestTaskAfterGreetingGetsTools(t *testing.T) {
	host := &captureHost{}
	m := conversationModel(t, host)
	send(t, m, host, "hello")
	m.history = append(m.history, api.Message{Role: "assistant", Content: "Hi! What would you like to work on?"})
	send(t, m, host, "build a snake game that runs in the browser")

	if m.clarificationOnly || len(host.last(t).Tools) == 0 {
		t.Fatalf("the stated task was still treated as a greeting (tools=%d)", len(host.last(t).Tools))
	}
}

// The loop in the log: answering an ask_user picker never cleared the latch,
// so every later request offered ask_user alone.
func TestAnsweringAQuestionClearsClarification(t *testing.T) {
	for answer, want := range map[string]bool{
		"I want you to build a snake game that plays in the browser": false,
		"hi": true,
	} {
		m := statusTestModel()
		m.clarificationOnly = true
		m.history = append(m.history,
			api.Message{Role: "assistant", ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "ask_user"}}}},
			api.Message{Role: "tool", ToolName: "ask_user", Content: "QUESTION: what?"})
		m.question = tools.AskUserQuestion{Question: "what?", Options: []string{"a"}}
		m.questionResult = len(m.history) - 1

		m.applyQuestionAnswer(answer)

		if m.clarificationOnly != want {
			t.Errorf("answer %q: clarificationOnly = %v, want %v", answer, m.clarificationOnly, want)
		}
	}
}

func TestPromptsSendQuestionsToChat(t *testing.T) {
	for name, prompt := range map[string]string{"full": systemPrompt, "compact": compactSystemPrompt} {
		if !strings.Contains(prompt, "plain text") || !strings.Contains(prompt, "Use ask_user only") {
			t.Errorf("%s prompt still routes every question through ask_user", name)
		}
	}
}

func TestNeedsTaskClarification(t *testing.T) {
	greetings := []string{"hello", "hi", "hey there", "Hey there!", "good morning", "hi Layla",
		"hello, how are you?", "hiya", "yo", "hey there, I have a task for you", "Hello i have a task for you"}
	tasks := []string{"hide the button", "hi, fix the parser", "hello, find the snake code", "hey there, build a game",
		"yo dawg add tests", "howdy do you parse json", "high priority: fix login", "Can you help me fix the failing auth test?"}
	for _, g := range greetings {
		if !needsTaskClarification(g) {
			t.Errorf("greeting %q was treated as a task", g)
		}
	}
	for _, task := range tasks {
		if needsTaskClarification(task) {
			t.Errorf("task %q was treated as a greeting", task)
		}
	}
}

// A message queued while the model worked answers the picker it then opens,
// instead of waiting in the queue while the picker waits for a key.
func TestQueuedMessageAnswersPausedQuestion(t *testing.T) {
	host := &captureHost{}
	m := conversationModel(t, host)
	m.history = append(m.history,
		api.Message{Role: "user", Content: "pick a colour"},
		api.Message{Role: "assistant", ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: "ask_user"}}}},
		api.Message{Role: "tool", ToolName: "ask_user", Content: "QUESTION: Which colour?"})
	m.question = tools.AskUserQuestion{Question: "Which colour?", Options: []string{"red", "blue"}}
	m.questionResult = len(m.history) - 1
	m.state = stateQuestion
	m.queue = []string{"blue"}

	asked := m.questionResult
	if cmd := m.answerPauseFromQueue(); cmd == nil {
		t.Fatal("the paused turn did not resume")
	}
	if got := m.history[asked].Content; got != "ANSWER: blue" {
		t.Fatalf("tool result = %q, want the queued message as the answer", got)
	}
	if len(m.queue) != 0 || m.state != stateChat || m.phase != phaseStreaming {
		t.Fatalf("queue=%v state=%v phase=%s", m.queue, m.state, m.phase)
	}
	m.abandonTurnWork()
}

// The self-check must not send the model shopping for a runtime: asked to
// "run it" with no browser available, a real model downloaded Node.js.
func TestSelfCheckForbidsInstallingSoftware(t *testing.T) {
	if !strings.Contains(noCheckChallenge, "Do not download or install software") ||
		!strings.Contains(noCheckChallenge, "review the code") {
		t.Fatalf("self-check wording: %q", noCheckChallenge)
	}
}
