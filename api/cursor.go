package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

// The Cursor agent transport. Cursor publishes no inference endpoint, but it
// ships a local CLI, so this provider speaks to a subprocess instead of HTTP —
// no proxy, no tunnel, nothing to install beyond the CLI itself.
//
// Flags verified against `agent --help`, not documentation. Three matter:
//
//   - --plan is what makes this read-only. Print mode alone does NOT: the CLI's
//     own help says "-p ... Has access to all tools, including write and shell."
//     --plan is documented as "read-only/planning (analyze, propose plans, no
//     edits)". --force/--yolo are never passed.
//   - --trust is required, or a headless run aborts on the workspace-trust
//     prompt. It only suppresses that prompt; --plan still bars edits.
//   - --model takes no short form. `-m` is rejected outright.
//
// It is an agent, not a model: it reads and searches the workspace itself and
// speaks no tool protocol, so it is never offered OllamaCode's tools.
const ProviderCursor = "cursor"

// cursorCommandCandidates are the names the Cursor CLI installs under —
// installs have used both. Whichever is on PATH wins, preferring the
// unambiguous one because "agent" is a name anything could take.
var cursorCommandCandidates = []string{"cursor-agent", "agent"}

// IsCursor reports whether this host drives the cursor-agent CLI.
func (o OllamaHost) IsCursor() bool { return o.provider == ProviderCursor }

// cursorCommands lists the binaries to try, in order. An explicit path is the
// only candidate; otherwise both install names are tried and the first that
// actually starts wins.
func (o OllamaHost) cursorCommands() []string {
	if c := strings.TrimSpace(o.uri); c != "" {
		return []string{c}
	}
	return cursorCommandCandidates
}

// cursorEnv passes the API key through the environment rather than --api-key, so
// the secret never appears in the process list where any local user can read it.
func (o OllamaHost) cursorEnv() []string {
	if o.apiKey == "" {
		return nil // inherit an externally exported CURSOR_API_KEY
	}
	return append(os.Environ(), "CURSOR_API_KEY="+o.apiKey)
}

// cursorPrompt flattens the conversation into the single prompt the CLI takes.
// Roles are labelled so the agent can tell its own prior turns from the user's.
func cursorPrompt(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		switch m.Role {
		case "system":
			b.WriteString(text + "\n\n")
		case "assistant":
			b.WriteString("[you, earlier]: " + text + "\n\n")
		case "tool":
			b.WriteString("[tool result]: " + text + "\n\n")
		default:
			b.WriteString("[user]: " + text + "\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// cursorEvent is the subset of --output-format stream-json we consume, matching
// what the CLI actually emits:
//
//	{"type":"thinking","subtype":"delta","text":"…","timestamp_ms":…}
//	{"type":"assistant","message":{"content":[{"text":"…"}]},"timestamp_ms":…}
//	{"type":"assistant","message":{"content":[{"text":"…"}]}}   ← whole answer, repeated
//	{"type":"result","subtype":"success","result":"…","is_error":false}
type cursorEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Text    string `json:"text"` // thinking deltas carry text at the top level
	Message struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
	// TimestampMS is present on streaming deltas and absent on the consolidated
	// repeat. It is the only thing distinguishing them, and taking both would
	// double every answer.
	TimestampMS int64 `json:"timestamp_ms"`
}

func (e cursorEvent) text() string {
	var b strings.Builder
	for _, c := range e.Message.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// startCursor runs the first candidate binary that actually starts, so a setup
// providing only one of the two install names works without configuration.
func (o OllamaHost) startCursor(ctx context.Context, args []string) (*exec.Cmd, io.ReadCloser, *strings.Builder, error) {
	var lastErr error
	for _, bin := range o.cursorCommands() {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = o.cursorEnv()
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			lastErr = err
			continue
		}
		stderr := &strings.Builder{}
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			lastErr = err
			continue // this name isn't usable; try the next
		}
		return cmd, stdout, stderr, nil
	}
	return nil, nil, nil, cursorStartError(lastErr)
}

func (o OllamaHost) chatCursor(ctx context.Context, req ChatRequest) (<-chan ChatResponse, <-chan error) {
	respChan := make(chan ChatResponse)
	errChan := make(chan error, 1)

	go func() {
		defer close(respChan)
		defer close(errChan)

		// --plan keeps it read-only. --force / --yolo are never passed: those are
		// what would let it edit and run commands.
		args := []string{
			"-p", "--plan",
			"--output-format", "stream-json", "--stream-partial-output",
		}
		// --trust is opt-in per provider: it marks the working directory trusted
		// in Cursor without asking. Without it a headless run aborts, which is
		// the honest default — see cursorTrustError.
		if o.trustWorkspace {
			args = append(args, "--trust")
		}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if wd, err := os.Getwd(); err == nil {
			args = append(args, "--workspace", wd)
		}
		args = append(args, cursorPrompt(req.Messages))

		cmd, stdout, stderr, err := o.startCursor(ctx, args)
		if err != nil {
			errChan <- err
			return
		}

		emit := func(c ChatResponse) bool {
			select {
			case respChan <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var answered strings.Builder
		var sawDelta bool
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		for scanner.Scan() {
			var ev cursorEvent
			if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
				continue // progress lines that aren't events
			}

			switch ev.Type {
			case "thinking":
				if ev.Text == "" {
					continue
				}
				if !emit(ChatResponse{Message: Message{Role: "assistant", Thinking: ev.Text}}) {
					return
				}

			case "assistant":
				text := ev.text()
				if text == "" {
					continue
				}
				// Deltas carry timestamp_ms; the CLI then repeats the whole
				// message once without it. Take the deltas, or the repeat only
				// when no deltas arrived at all.
				if ev.TimestampMS != 0 {
					sawDelta = true
				} else if sawDelta {
					continue
				}
				answered.WriteString(text)
				if !emit(ChatResponse{Message: Message{Role: "assistant", Content: text}}) {
					return
				}

			case "result":
				if ev.IsError {
					errChan <- fmt.Errorf("cursor agent failed: %s", cursorStderr(firstNonEmpty(ev.Result, stderr.String())))
					return
				}
				// Only useful when nothing streamed — otherwise it repeats the
				// answer already delivered.
				if answered.Len() == 0 && ev.Result != "" {
					answered.WriteString(ev.Result)
					if !emit(ChatResponse{Message: Message{Role: "assistant", Content: ev.Result}}) {
						return
					}
				}
			}
		}

		if err := cmd.Wait(); err != nil {
			select {
			case <-ctx.Done(): // cancelled turn, not a failure
				return
			default:
			}
			if answered.Len() == 0 {
				if e := cursorTrustError(stderr.String()); e != nil {
					errChan <- e
					return
				}
				errChan <- fmt.Errorf("cursor agent failed: %v: %s", err, cursorStderr(stderr.String()))
				return
			}
			// Partial output already reached the user; end the turn with it.
		}

		emit(ChatResponse{Done: true, Message: Message{Role: "assistant"}})
	}()

	return respChan, errChan
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// chatOnceCursor drains the streaming path, since the CLI has no separate
// non-streaming mode. Constrained decoding (req.Format) has no equivalent.
func (o OllamaHost) chatOnceCursor(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	resp, errs := o.chatCursor(ctx, req)
	var b strings.Builder
	for c := range resp {
		b.WriteString(c.Message.Content)
	}
	if err := <-errs; err != nil {
		return ChatResponse{}, err
	}
	return ChatResponse{
		Model:   req.Model,
		Done:    true,
		Message: Message{Role: "assistant", Content: b.String()},
	}, nil
}

// modelsCursor lists what the signed-in Cursor account can use, so the model
// picker works for this provider like any other.
//
// The output is a header line, a blank, then one "<id> - <Description>" per
// line. Splitting on " - " is the whole parse; an earlier version dropped every
// line containing a space and therefore found nothing.
func (o OllamaHost) modelsCursor() (*ModelListResponse, error) {
	var out []byte
	var lastErr error
	for _, bin := range o.cursorCommands() {
		cmd := exec.Command(bin, "--list-models")
		cmd.Env = o.cursorEnv()
		b, err := cmd.Output()
		if err != nil {
			lastErr = err
			continue
		}
		out, lastErr = b, nil
		break
	}
	if lastErr != nil {
		return nil, cursorStartError(lastErr)
	}

	var list ModelListResponse
	for _, line := range strings.Split(string(out), "\n") {
		id, _, ok := strings.Cut(strings.TrimSpace(line), " - ")
		if !ok {
			continue // header and blank lines have no separator
		}
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		list.Models = append(list.Models, struct {
			Name string `json:"name"`
		}{Name: id})
	}
	if len(list.Models) == 0 {
		return nil, fmt.Errorf("--list-models returned no models; is the CLI signed in? (run `%s login`)", o.cursorCommands()[0])
	}
	return &list, nil
}

// cursorStartError turns "executable file not found" into the one instruction
// that actually fixes it, naming both install names since either may be the one
// missing.
func cursorStartError(err error) error {
	if err == nil {
		return fmt.Errorf("could not start the Cursor agent")
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Cursor agent not found — it installs as %s; install it (https://cursor.com/docs/cli) or set this provider's command to its full path",
			strings.Join(cursorCommandCandidates, " or "))
	}
	return fmt.Errorf("cursor agent: %v", err)
}

// cursorTrustError recognizes the workspace-trust abort and returns an
// actionable message, or nil when that is not the failure. Worth special-casing:
// the CLI's own advice is to pass --yolo or -f, which would also hand it write
// and shell access and defeat the point of routing planning here.
func cursorTrustError(stderr string) error {
	if !strings.Contains(strings.ToLower(stderr), "workspace trust") {
		return nil
	}
	return fmt.Errorf("Cursor will not run here: this workspace is not trusted. " +
		"Turn on Trust for this provider in /provider, or run `agent` once interactively in this directory and accept. " +
		"Do not use --yolo or -f as the CLI suggests — those also grant write and shell access")
}

// cursorStderr trims the CLI's error output to something that fits a toast.
func cursorStderr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no error output"
	}
	if lines := strings.Split(s, "\n"); len(lines) > 3 {
		s = strings.Join(lines[len(lines)-3:], "; ")
	}
	return s
}
