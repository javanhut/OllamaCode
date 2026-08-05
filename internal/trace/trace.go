package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Time       time.Time       `json:"time"`
	Kind       string          `json:"kind"`
	Turn       int             `json:"turn,omitempty"`
	Model      string          `json:"model,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Result     string          `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Metadata   map[string]any  `json:"metadata,omitempty"`
}

type Recorder struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

type PromotedFixture struct {
	Name          string         `json:"name"`
	Prompt        string         `json:"prompt,omitempty"`
	RequiredTools []string       `json:"required_tools,omitempty"`
	Calls         []PromotedCall `json:"calls,omitempty"`
}

type PromotedCall struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func Open(path string) (*Recorder, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("trace path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Recorder{file: f, enc: json.NewEncoder(f)}, nil
}

func (r *Recorder) Record(event Event) error {
	if r == nil {
		return nil
	}
	event.Time = time.Now().UTC()
	event.Arguments = RedactJSON(event.Arguments)
	event.Payload = RedactJSON(event.Payload)
	event.Result = RedactText(event.Result)
	event.Error = RedactText(event.Error)
	if event.Metadata != nil {
		if raw, err := json.Marshal(event.Metadata); err == nil {
			redacted := RedactJSON(raw)
			_ = json.Unmarshal(redacted, &event.Metadata)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enc.Encode(event)
}

func (r *Recorder) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}

var secretKey = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|authorization|cookie)`)
var bearer = regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/-]+`)

func RedactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return json.RawMessage(strconvQuote(RedactText(string(raw))))
	}
	value = redactValue(value)
	out, _ := json.Marshal(value)
	return out
}

func redactValue(value any) any {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if secretKey.MatchString(key) {
				current[key] = "[REDACTED]"
			} else {
				current[key] = redactValue(child)
			}
		}
	case []any:
		for i, child := range current {
			current[i] = redactValue(child)
		}
	case string:
		return RedactText(current)
	}
	return value
}

func RedactText(value string) string { return bearer.ReplaceAllString(value, "Bearer [REDACTED]") }

func strconvQuote(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

// Replay reads a trace in order. A callback lets tests or fixture-promotion
// tooling feed events through the shared harness without a live provider.
func Replay(path string, consume func(Event) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if err := consume(event); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// Promote converts a redacted real trace into an eval-fixture skeleton. The
// behavioral check remains deliberately empty because only a human can define
// the correct workspace outcome; recorded tool use is evidence, not truth.
func Promote(path string) (PromotedFixture, error) {
	fixture := PromotedFixture{Name: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))}
	seen := map[string]bool{}
	err := Replay(path, func(event Event) error {
		if event.Kind == "turn_start" && fixture.Prompt == "" {
			if prompt, ok := event.Metadata["task"].(string); ok {
				fixture.Prompt = prompt
			}
		}
		if event.Kind == "tool" && event.Tool != "" {
			fixture.Calls = append(fixture.Calls, PromotedCall{Tool: event.Tool, Arguments: event.Arguments})
			if !seen[event.Tool] {
				fixture.RequiredTools = append(fixture.RequiredTools, event.Tool)
				seen[event.Tool] = true
			}
		}
		return nil
	})
	return fixture, err
}
