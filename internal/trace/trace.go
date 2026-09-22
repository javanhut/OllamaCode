package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
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

	// State for RecordRequest's delta logging.
	lastSigs   []uint64 // per-message hashes of the previous request
	lastTools  uint64   // hash of the previous tool schemas
	lastFormat uint64   // hash of the previous constraint grammar
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
	return open(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
}

// OpenFresh creates a private trace file for one debug session, replacing any
// previous file at the same path. A single-session log is much easier to hand
// to another model than an append-only mixture of unrelated runs.
func OpenFresh(path string) (*Recorder, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to replace symlinked trace path %s", path)
	}
	return open(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
}

func open(path string, flags int) (*Recorder, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("trace path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
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

// RecordRequest writes a model_request event carrying only what changed since
// the previous one: the messages appended after the longest common prefix, and
// the tool schemas and constraint grammar only when they change. Re-logging the
// whole conversation plus every tool schema and the derived format grammar on
// every turn made traces grow quadratically — one 27-turn session cost 1.2 MB,
// ~90% of it the same bytes over and over.
//
// The comparison is per message rather than whole-prefix because the assembled
// history is not append-only: the volatile mode banner is rebuilt and re-sent
// last on every turn, so the final message always differs.
//
// metadata.payload_from is the index of the first message in the payload, so a
// reader can splice deltas back into a full conversation; it is absent (0) when
// the payload is the whole list, which is what happens whenever the prefix
// changed underneath us (compaction, history eviction). tool_definitions and
// format are absent when unchanged since the last request in the same trace.
func (r *Recorder) RecordRequest(event Event, msgs []api.Message, defs []tools.Tool) error {
	if r == nil {
		return nil
	}
	if event.Metadata == nil {
		event.Metadata = map[string]any{}
	}
	sigs := messageSignatures(msgs)
	toolHash := hashValue(defs)
	formatHash := hashValue(event.Metadata["format"])

	r.mu.Lock()
	from := commonPrefix(r.lastSigs, sigs)
	r.lastSigs = sigs
	sameTools := toolHash == r.lastTools
	sameFormat := formatHash == r.lastFormat
	r.lastTools, r.lastFormat = toolHash, formatHash
	r.mu.Unlock()

	event.Kind = "model_request"
	event.Payload, _ = json.Marshal(msgs[from:])
	event.Metadata["message_count"] = len(msgs)
	if from > 0 {
		event.Metadata["payload_from"] = from
	}
	if !sameTools {
		event.Metadata["tool_definitions"] = defs
	}
	if sameFormat {
		delete(event.Metadata, "format")
	}
	return r.Record(event)
}

func messageSignatures(msgs []api.Message) []uint64 {
	sigs := make([]uint64, len(msgs))
	for i := range msgs {
		sigs[i] = hashValue(msgs[i])
	}
	return sigs
}

func commonPrefix(a, b []uint64) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func hashValue(value any) uint64 {
	h := fnv.New64a()
	_ = json.NewEncoder(h).Encode(value)
	return h.Sum64()
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
var secretAssignment = regexp.MustCompile(`(?im)\b([a-z0-9_]*(?:api[_-]?key|token|secret|password|authorization|cookie)[a-z0-9_]*)\s*(?::=|=|:)\s*([^\s,;]+)`)
var commonSecret = regexp.MustCompile(`\b(?:sk-[a-zA-Z0-9_-]{16,}|ghp_[a-zA-Z0-9]{16,}|github_pat_[a-zA-Z0-9_]{16,})\b`)

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
			// A number under a secret-shaped key is metering, not a credential:
			// prompt_tokens/completion_tokens matched "token" and got redacted,
			// which blinded token accounting while every file the model read
			// stayed in cleartext. Only strings can carry a secret.
			if secretKey.MatchString(key) && !isNumberOrBool(child) {
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

func isNumberOrBool(value any) bool {
	switch value.(type) {
	case float64, int, int64, bool, json.Number:
		return true
	}
	return false
}

func RedactText(value string) string {
	value = bearer.ReplaceAllString(value, "Bearer [REDACTED]")
	value = secretAssignment.ReplaceAllString(value, "$1=[REDACTED]")
	return commonSecret.ReplaceAllString(value, "[REDACTED]")
}

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
