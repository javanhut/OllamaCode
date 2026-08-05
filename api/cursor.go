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

// cursorEvent is the subset of --output-format stream-json we consume.
//
// The shapes below are captured from real runs, not documentation. The
// important one is the last: in --plan mode the assistant/result stream carries
// only PROGRESS NARRATION ("Exploring tui/route.go…"). The actual plan is
// delivered as an interaction_query, and reading the wrong one is why a plan
// that named files looked like prose that named none.
//
//	{"type":"thinking","subtype":"delta","text":"…"}
//	{"type":"assistant","message":{"content":[{"text":"…"}]}}
//	{"type":"result","subtype":"success","result":"…","is_error":false}
//	{"type":"interaction_query","subtype":"request",
//	 "query_type":"createPlanRequestQuery",
//	 "query":{"createPlanRequestQuery":{"args":{"plan":"# …"}}}}
type cursorEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Text    string `json:"text"` // thinking deltas carry text at the top level
	Message struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	QueryType string `json:"query_type"`
	Query     struct {
		CreatePlan struct {
			Args struct {
				Plan string `json:"plan"`
			} `json:"args"`
		} `json:"createPlanRequestQuery"`
		AskQuestion struct {
			Args struct {
				Title string `json:"title"`
			} `json:"args"`
		} `json:"askQuestionInteractionQuery"`
	} `json:"query"`
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
		//
		// --stream-partial-output is deliberately NOT passed. With it, each
		// message arrives as fragments and is then repeated whole — and the repeat
		// carries a timestamp just like the fragments, so there is no reliable way
		// to tell them apart. Every answer came out doubled.
		args := []string{"-p", "--plan", "--output-format", "stream-json"}
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

		var plan, question, narration strings.Builder
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		for scanner.Scan() {
			var ev cursorEvent
			if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
				continue // progress lines that aren't events
			}

			switch ev.Type {
			case "thinking":
				if ev.Text != "" && !emit(ChatResponse{Message: Message{Role: "assistant", Thinking: ev.Text}}) {
					return
				}

			case "assistant":
				// Narration, not the answer — route it to the reasoning ticker so
				// the transcript is left holding the plan alone.
				if t := ev.text(); t != "" && !emit(ChatResponse{Message: Message{Role: "assistant", Thinking: t}}) {
					return
				}

			case "interaction_query":
				if ev.Subtype != "request" {
					continue
				}
				switch ev.QueryType {
				case "createPlanRequestQuery":
					plan.WriteString(ev.Query.CreatePlan.Args.Plan)
				case "askQuestionInteractionQuery":
					// Headless runs auto-skip these, so the question is otherwise
					// lost and the turn looks like it produced nothing.
					question.WriteString(ev.Query.AskQuestion.Args.Title)
				}

			case "result":
				if ev.IsError {
					errChan <- fmt.Errorf("cursor agent failed: %s", cursorStderr(firstNonEmpty(ev.Result, stderr.String())))
					return
				}
				narration.WriteString(ev.Result)
			}
		}

		if err := cmd.Wait(); err != nil {
			select {
			case <-ctx.Done(): // cancelled turn, not a failure
				return
			default:
			}
			if e := cursorTrustError(stderr.String()); e != nil {
				errChan <- e
				return
			}
			errChan <- fmt.Errorf("cursor agent failed: %v: %s", err, cursorStderr(stderr.String()))
			return
		}

		if answer := cursorAnswer(plan.String(), question.String(), narration.String()); answer != "" {
			if !emit(ChatResponse{Message: Message{Role: "assistant", Content: answer}}) {
				return
			}
		}
		emit(ChatResponse{Done: true, Message: Message{Role: "assistant"}})
	}()

	return respChan, errChan
}

// cursorAnswer picks what the turn actually produced. The plan is the answer
// when there is one; a clarifying question the headless run auto-skipped is the
// next most useful thing to report; narration is the last resort.
func cursorAnswer(plan, question, narration string) string {
	if p := strings.TrimSpace(plan); p != "" {
		return p
	}
	q := strings.TrimSpace(question)
	n := strings.TrimSpace(narration)
	if q == "" {
		return n
	}
	answer := "No plan was produced — the planner asked for more information first: " + q
	if n != "" {
		answer += "\n\n" + n
	}
	return answer
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
		list.Models = append(list.Models, ModelSummary{Name: id})
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
