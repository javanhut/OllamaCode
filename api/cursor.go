package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

// The cursor-agent transport. Cursor publishes no inference endpoint, but it
// ships a local CLI, so this provider speaks to a subprocess instead of HTTP —
// no proxy, no tunnel, nothing to install beyond cursor-agent itself.
//
// Two properties of the CLI shape the design:
//
//   - It is an agent, not a model. It reads and searches the workspace on its
//     own, so it is never offered OllamaCode's tools; it answers in prose.
//   - In print mode WITHOUT --force it only proposes file changes, never applies
//     them. That default is load-bearing: it is what makes routing plan mode to
//     cursor-agent safe. --force is deliberately never passed.
const ProviderCursor = "cursor"

// DefaultCursorCommand is the binary looked up on PATH when a provider doesn't
// name one.
const DefaultCursorCommand = "cursor-agent"

// IsCursor reports whether this host drives the cursor-agent CLI.
func (o OllamaHost) IsCursor() bool { return o.provider == ProviderCursor }

// cursorCommand is the binary to run: the provider's configured URL field when
// set (a path), else whatever is on PATH.
func (o OllamaHost) cursorCommand() string {
	if c := strings.TrimSpace(o.uri); c != "" {
		return c
	}
	return DefaultCursorCommand
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

// cursorEvent is the subset of --output-format stream-json we consume. Text
// arrives on assistant events; everything else (tool_call, system/init) is
// progress the caller doesn't need.
type cursorEvent struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Result string `json:"result"`
}

func (o OllamaHost) chatCursor(ctx context.Context, req ChatRequest) (<-chan ChatResponse, <-chan error) {
	respChan := make(chan ChatResponse)
	errChan := make(chan error, 1)

	go func() {
		defer close(respChan)
		defer close(errChan)

		args := []string{"-p", "--output-format", "stream-json", "--stream-partial-output"}
		if req.Model != "" {
			args = append(args, "-m", req.Model)
		}
		if wd, err := os.Getwd(); err == nil {
			args = append(args, "--workspace", wd)
		}
		// No --force: the CLI then proposes edits instead of applying them, which
		// is what keeps a routed planning model from writing to the repo behind
		// OllamaCode's permission prompts and /undo checkpoints.
		args = append(args, cursorPrompt(req.Messages))

		cmd := exec.CommandContext(ctx, o.cursorCommand(), args...)
		cmd.Env = o.cursorEnv()

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			errChan <- fmt.Errorf("cursor-agent: %v", err)
			return
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr

		if err := cmd.Start(); err != nil {
			errChan <- cursorStartError(o.cursorCommand(), err)
			return
		}

		var last strings.Builder
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var ev cursorEvent
			if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
				continue // progress lines that aren't events
			}
			var text string
			switch ev.Type {
			case "assistant":
				for _, c := range ev.Message.Content {
					text += c.Text
				}
			case "result":
				// The final event repeats the whole answer; emit it only if the
				// deltas produced nothing, so the text isn't duplicated.
				if last.Len() == 0 {
					text = ev.Result
				}
			}
			if text == "" {
				continue
			}
			last.WriteString(text)
			select {
			case respChan <- ChatResponse{Message: Message{Role: "assistant", Content: text}}:
			case <-ctx.Done():
				return
			}
		}

		if err := cmd.Wait(); err != nil {
			select {
			case <-ctx.Done(): // cancelled turn, not a failure
				return
			default:
			}
			if last.Len() == 0 {
				errChan <- fmt.Errorf("cursor-agent failed: %v: %s", err, cursorStderr(stderr.String()))
				return
			}
			// Partial output already reached the user; end the turn with it.
		}

		select {
		case respChan <- ChatResponse{Done: true, Message: Message{Role: "assistant"}}:
		case <-ctx.Done():
		}
	}()

	return respChan, errChan
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
func (o OllamaHost) modelsCursor() (*ModelListResponse, error) {
	cmd := exec.Command(o.cursorCommand(), "--list-models")
	cmd.Env = o.cursorEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, cursorStartError(o.cursorCommand(), err)
	}
	var list ModelListResponse
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		// Skip blanks, bullet markers and any header line the CLI prints.
		name = strings.TrimLeft(name, "-*• ")
		if name == "" || strings.HasSuffix(name, ":") || strings.Contains(name, " ") {
			continue
		}
		list.Models = append(list.Models, struct {
			Name string `json:"name"`
		}{Name: name})
	}
	if len(list.Models) == 0 {
		return nil, fmt.Errorf("cursor-agent --list-models returned no models; is it signed in? (run `%s login`)", o.cursorCommand())
	}
	return &list, nil
}

// cursorStartError turns "executable file not found" into the one instruction
// that actually fixes it.
func cursorStartError(bin string, err error) error {
	// ErrNotFound is a bare name missing from PATH; ErrNotExist is a configured
	// path that doesn't exist. Same advice, two different wrapped errors.
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s is not on PATH — install the Cursor CLI (https://cursor.com/docs/cli) or set this provider's command to its full path", bin)
	}
	return fmt.Errorf("%s: %v", bin, err)
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
