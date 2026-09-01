package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/javanhut/ollama_code/internal/jobs"
)

// Persistent terminal sessions. run_shell feeds a one-shot stdin string and
// never reads back, so every program that expects a keyboard — python/node
// REPLs, ssh, `git rebase -i`, scaffolding prompts — is out of reach or hangs.
// A session here is a real pty with a shell on the far end that outlives the
// tool call, driven by terminal_send and drained by terminal_read.
//
// The load-bearing detail, copied deliberately: wait_reason is a fact about why
// THIS call stopped waiting, reported independently of alive and exit_code.
// Folding "output went quiet" into "the process exited" is the standard bug in
// naive pty tools — a REPL sitting at its prompt is the normal case, not an
// ending.

const (
	terminalDefaultWait = 2 * time.Second
	terminalMaxWait     = 60 * time.Second
	// How long output has to stop before a send calls it quiet. Short enough
	// that an `echo` round-trips fast, long enough to survive the gap between a
	// program's output and its prompt.
	terminalQuiet     = 300 * time.Millisecond
	terminalBufferMax = 256 * 1024
	// A looping model must not be able to fork-bomb the box with shells.
	terminalMaxSessions = 8
)

// Why a send stopped waiting. Three distinct facts, never folded together:
// only session_exit means the shell is gone.
const (
	waitSilence     = "silence"
	waitTimeout     = "timeout"
	waitSessionExit = "session_exit"
)

// terminalDroppedNotice rides a buffer that overflowed, so the model never
// reads a silently spliced stream as a continuous one.
var terminalDroppedNotice = fmt.Sprintf("[earlier output dropped: this session buffers only the most recent %d KiB]", terminalBufferMax/1024)

// terminal is one live pty session. It mirrors bgJob (tools/shell_bg.go) where
// the concepts line up — same registry hookup, same exit bookkeeping — and
// differs only in owning a pty instead of a pipe.
type terminal struct {
	id      int
	name    string
	command string
	pid     int
	started time.Time

	ptmx *os.File
	cmd  *exec.Cmd
	rec  *jobs.Job

	// sendMu serializes terminal_send on one session: two parallel sends into
	// one bash interleave keystrokes into a line neither caller typed.
	sendMu sync.Mutex

	// mu guards the buffer and the exit record. Short critical sections only —
	// it is never held across a wait, or the reader would block behind a send
	// that is waiting for the reader.
	mu       sync.Mutex
	buf      []byte
	dropped  bool
	closing  bool
	exitCode int
	exitErr  string

	wrote  chan struct{} // buffered(1), pulsed once per chunk read
	exited chan struct{} // closed once the child is REAPED, not merely signalled
}

var (
	// termMu guards ONLY the map. Look up under termMu, release it, then take
	// t.mu / t.sendMu — never the other order.
	termMu    sync.Mutex
	terminals = map[int]*terminal{}
)

// openTerminal starts command on a pty and registers the session. Modelled on
// startBackgroundShell: same sandbox-wrapped command, same registry hookup,
// same "a goroutine owns the lifecycle" shape.
func openTerminal(command, workingDir, name string) (*terminal, error) {
	if strings.TrimSpace(command) == "" {
		command = defaultLoginShell()
	}

	termMu.Lock()
	// Sessions that have exited are pruned here rather than kept forever: their
	// tail was already drained by the send that saw session_exit, and holding
	// them would let dead 256 KiB buffers accumulate without bound. Pruning on
	// open also bounds the map at terminalMaxSessions entries.
	for id, t := range terminals {
		if !t.alive() {
			delete(terminals, id)
		}
	}
	live := len(terminals)
	termMu.Unlock()
	if live >= terminalMaxSessions {
		return nil, fmt.Errorf("too many open terminal sessions (%d) — close one with terminal_close before opening another", live)
	}

	cmd := newShellCommand(command) // OS sandbox wrap + credential scrub, one call
	// NOT configureShellCommand: startPTY sets Setsid itself, and Setsid with
	// Setpgid is rejected by the kernel. See terminal_pty_unix.go.
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	// Readline and curses colour escapes are noise to a model reading the
	// buffer, so the session runs dumb. The inherited TERM is removed rather
	// than shadowed by a second entry: a duplicate TERM= in the environment is
	// not reliably last-wins in execve. Known ceiling — full-screen TUIs (vim,
	// htop) will not render under TERM=dumb, which is the intended failure for
	// a tool meant to drive REPLs and prompts.
	cmd.Env = append(slices.DeleteFunc(cmd.Env, func(e string) bool {
		return strings.HasPrefix(e, "TERM=")
	}), "TERM=dumb")

	ptmx, err := startPTY(cmd)
	if err != nil {
		return nil, err
	}

	t := &terminal{
		name:    strings.TrimSpace(name),
		command: command,
		pid:     cmd.Process.Pid,
		started: time.Now(),
		ptmx:    ptmx,
		cmd:     cmd,
		wrote:   make(chan struct{}, 1),
		exited:  make(chan struct{}),
	}
	// The session id IS the unified job-registry id (internal/jobs), shared
	// with background shells and sub-agents — so job_list shows terminals and
	// job_kill closes them without a second id space to disambiguate.
	t.rec = jobs.Default().Register(jobs.KindTerminal, t.label(), jobs.Hooks{
		Status: t.statusLine,
		// peek, not take: job_output must never steal terminal_read's bytes.
		Output: func() string { return t.peek() },
		Cancel: func() { t.close() },
	})
	t.id = t.rec.ID()

	termMu.Lock()
	terminals[t.id] = t
	termMu.Unlock()

	go t.run()
	return t, nil
}

// defaultLoginShell is the user's shell, so a session behaves the way their
// own terminal does (aliases, prompt, rc files).
func defaultLoginShell() string {
	if sh := strings.TrimSpace(os.Getenv("SHELL")); sh != "" {
		return sh
	}
	return "sh"
}

// run is the session's whole lifecycle: drain the pty in a child goroutine,
// reap, record, announce. Nothing else calls cmd.Wait.
//
// The reap deliberately does NOT wait for the read loop. A read on the master
// returns when the LAST slave fd closes, not when the shell exits — SIGHUP on
// the controlling process's death reaches only the foreground process group —
// so one background job the shell leaves behind (`sleep 300 &`, a dev server)
// keeps this pty readable indefinitely. Reaping after the loop, as this used
// to, left that shell a zombie, alive() answering true for the life of ocode,
// terminal_send never reporting the session_exit it promises, and eight such
// sessions wedging terminal_open. Same wait-goroutine shape runShellCommand
// already uses for the same reason (shell_exec.go).
func (t *terminal) run() {
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		b := make([]byte, 8192)
		for {
			n, err := t.ptmx.Read(b)
			if n > 0 {
				t.append(b[:n])
			}
			// Any read error ends the drain: darwin reports EOF and linux EIO
			// once every slave fd is gone.
			if err != nil {
				return
			}
		}
	}()

	err := t.cmd.Wait()
	// Reaped. Let the reader pick up the child's dying words — that grace is
	// what keeps a session_exit drain complete — then hang up. A read deadline
	// cannot do the hanging up: creack opens /dev/ptmx blocking, so os.NewFile
	// hands back an unpollable *os.File whose SetReadDeadline is "file type
	// does not support deadline" and whose Close does not interrupt an
	// in-flight Read. Killing the group does, and it is safe after the reap
	// precisely here: the only reason the reader is still parked is that a
	// descendant holds the slave open, and a live group member keeps the dead
	// leader's pid allocated as its pgid, so the signal cannot reach a recycled
	// pid.
	select {
	case <-drained:
	case <-time.After(terminalQuiet):
		killShellCommand(t.cmd)
	}
	t.ptmx.Close() // nothing else closes the master when a session ends by itself
	t.mu.Lock()
	if ee, ok := err.(*exec.ExitError); ok {
		t.exitCode = ee.ExitCode()
	} else if err != nil {
		t.exitErr = err.Error()
	}
	exitCode, exitErr := t.exitCode, t.exitErr
	t.mu.Unlock()
	close(t.exited)

	switch {
	case exitErr != "":
		t.rec.Fail(exitErr)
	case exitCode != 0:
		t.rec.Fail(fmt.Sprintf("exit %d", exitCode))
	default:
		t.rec.Finish("exit 0")
	}
}

func (t *terminal) append(p []byte) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	// ponytail: slice trim, one memmove per overflowing write. A real ring
	// buffer only if a session is ever observed streaming hard enough to care.
	if over := len(t.buf) - terminalBufferMax; over > 0 {
		t.buf = t.buf[over:]
		t.dropped = true
	}
	t.mu.Unlock()
	// Pulsed outside the lock: wait() holds no lock, and the reader must never
	// block on a receiver that is about to take t.mu.
	select {
	case t.wrote <- struct{}{}:
	default:
	}
}

// take drains the buffer. Callers get everything since the last drain, once.
func (t *terminal) take() string {
	t.mu.Lock()
	out, dropped := string(t.buf), t.dropped
	// nil, not [:0]: the backing array is up to terminalBufferMax and a drained
	// session has no reason to keep holding it.
	t.buf, t.dropped = nil, false
	t.mu.Unlock()
	return terminalText(out, dropped)
}

// peek is take without the drain, for the registry's Output hook.
func (t *terminal) peek() string {
	t.mu.Lock()
	out, dropped := string(t.buf), t.dropped
	t.mu.Unlock()
	return terminalText(out, dropped)
}

// terminalText normalizes the CRLF a pty's line discipline emits, so the model
// reads ordinary lines rather than a stream of stray carriage returns.
func terminalText(out string, dropped bool) string {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	if dropped {
		return terminalDroppedNotice + "\n" + out
	}
	return out
}

// wait blocks until the session goes quiet, the budget runs out, or the child
// exits, and reports which. It never touches the buffer: the caller drains
// separately, so the reason and the bytes are independent facts.
func (t *terminal) wait(budget time.Duration) string {
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	quiet := time.NewTimer(terminalQuiet)
	defer quiet.Stop()
	sawOutput := false
	for {
		select {
		case <-t.exited:
			return waitSessionExit
		case <-t.wrote:
			sawOutput = true
			// Go 1.23+ Reset is race-free on a timer whose channel may hold a
			// stale send; do not reintroduce a stop/drain dance around it.
			quiet.Reset(terminalQuiet)
		case <-quiet.C:
			// The guard is the whole point. Quiet with nothing produced yet is
			// NOT the end of the command — a compile prints nothing for
			// seconds. Only silence AFTER output means the program is sitting
			// at its prompt; reporting the other case as an ending is the
			// standard naive-pty bug.
			if sawOutput {
				return waitSilence
			}
			quiet.Reset(terminalQuiet)
		case <-deadline.C:
			return waitTimeout
		}
	}
}

func (t *terminal) alive() bool {
	select {
	case <-t.exited:
		return false
	default:
		return true
	}
}

// ended reports a session no send can reach any more: exited, or torn down by
// a close that has not finished reaping yet.
func (t *terminal) ended() bool {
	t.mu.Lock()
	closing := t.closing
	t.mu.Unlock()
	return closing || !t.alive()
}

func (t *terminal) label() string {
	if t.name != "" {
		return t.name
	}
	return shortCommand(t.command)
}

func (t *terminal) statusLine() string {
	if t.alive() {
		return fmt.Sprintf("alive (pid %d, %s elapsed)", t.pid, time.Since(t.started).Round(time.Second))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exitErr != "" {
		return "exited (" + t.exitErr + ")"
	}
	return fmt.Sprintf("exited %d", t.exitCode)
}

// header is the result's first line, shaped like renderJobOutput's so it stays
// greppable and survives truncateResult's head/tail cut. The three orthogonal
// facts get three named fields on one line and are never folded into one word.
// reason is empty for terminal_read, which waited on nothing.
func (t *terminal) header(reason string) string {
	var b strings.Builder
	// alive comes from ended(), not alive(): a session inside close()'s reap
	// window is one no send can reach, and printing alive=true beside
	// wait_reason=session_exit contradicts the one fact session_exit carries.
	fmt.Fprintf(&b, "terminal %d: alive=%v", t.id, !t.ended())
	if reason != "" {
		fmt.Fprintf(&b, " wait_reason=%s", reason)
	}
	// The exit fields wait for the actual reap: mid-teardown there is no exit
	// status yet, and printing the zero value would invent one.
	if !t.alive() {
		t.mu.Lock()
		exitCode, exitErr := t.exitCode, t.exitErr
		t.mu.Unlock()
		fmt.Fprintf(&b, " exit_code=%d", exitCode)
		if exitErr != "" {
			fmt.Fprintf(&b, " error=%s", exitErr)
		}
	}
	return b.String()
}

// close tears the session down and waits for the child to actually be gone.
// The order matters and each step earns its place:
//  1. mark closing, so a concurrent send takes the ended branch instead of
//     writing into an fd being torn down;
//  2. close the pty, hanging up the child's controlling terminal;
//  3. SIGKILL the group, since a hangup alone is a request — and since the
//     master is an unpollable *os.File, the kill is also the only thing that
//     ends an in-flight read on it (see run);
//  4. await the reap. Step 4 is the difference between signalling and
//     quiescence, and the reason close exists as its own function.
//
// Idempotent: a second close on an already-exited session returns immediately.
func (t *terminal) close() {
	t.mu.Lock()
	t.closing = true
	t.mu.Unlock()
	t.ptmx.Close()
	select {
	case <-t.exited:
		// Already reaped. Killing now would send SIGKILL to whatever process
		// group inherited the pid in the meantime.
	default:
		killShellCommand(t.cmd)
	}
	<-t.exited
}

// CloseTerminals shuts down every live session. Wired to session exit like
// CloseLSP: without it the shells outlive the process that started them.
func CloseTerminals() {
	termMu.Lock()
	live := make([]*terminal, 0, len(terminals))
	for _, t := range terminals {
		live = append(live, t)
	}
	clear(terminals)
	termMu.Unlock()
	for _, t := range live {
		t.close()
	}
}

func lookupTerminal(id int) (*terminal, error) {
	termMu.Lock()
	t, ok := terminals[id]
	termMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no terminal %d (use terminal_list to list open sessions)", id)
	}
	return t, nil
}

// terminalWait is the send's own budget, clamped. Mirrors shellCallTimeout:
// one copy of the default/cap arithmetic, shared with the outer deadline
// tools/policy.go arms on this tool.
func terminalWait(ms float64) time.Duration {
	if ms <= 0 {
		return terminalDefaultWait
	}
	return min(time.Duration(ms*float64(time.Millisecond)), terminalMaxWait)
}

// TerminalOpenTool starts a persistent pty session.
func TerminalOpenTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "terminal_open",
			Description: "Open a persistent interactive terminal session and return its id. The session stays alive across tool calls until you close it.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"command":     {Type: "string", Description: "Program to run in the session. Defaults to your login shell ($SHELL, else sh). This is a PERSISTENT interactive session — use it for REPLs (python, node), ssh, `git rebase -i`, scaffolding prompts (npm create, cargo generate), anything that reads from a terminal. For a one-shot command use run_shell instead."},
					"working_dir": {Type: "string", Description: "Directory to start in. Must be inside the workspace."},
					"name":        {Type: "string", Description: "Short label shown by terminal_list."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Command    string `json:"command"`
				WorkingDir string `json:"working_dir"`
				Name       string `json:"name"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			// The command string itself is not confinable, but the directory it
			// starts in is — don't let working_dir teleport the shell elsewhere.
			if a.WorkingDir != "" {
				if err := jailCheck(a.WorkingDir); err != nil {
					return "", err
				}
			}
			t, err := openTerminal(a.Command, a.WorkingDir, a.Name)
			if err != nil {
				return "", err
			}
			return withSandboxNotice(fmt.Sprintf("terminal %d opened (pid %d): %s\nType into it with terminal_send({\"id\": %d, \"input\": \"...\"}); close it with terminal_close({\"id\": %d}).",
				t.id, t.pid, shortCommand(t.command), t.id, t.id)), nil
		},
	}
}

// TerminalSendTool types into a session and reports what came back.
func TerminalSendTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "terminal_send",
			Description: "Type input into an open terminal session and read what it prints back. The result header reports alive, wait_reason and exit_code separately. wait_reason=silence means output stopped and the program is sitting at its prompt — this is the NORMAL case and does NOT mean the session ended. wait_reason=timeout means it is still working; call terminal_read again later. Only wait_reason=session_exit means the shell is gone.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"id":      {Type: "number", Description: "Session id returned by terminal_open (see terminal_list)."},
					"input":   {Type: "string", Description: "Text to type. A trailing newline is added if you omit one, so \"echo hi\" submits the line; send \"\\n\" alone for a bare Enter, or \"\\u0003\" for Ctrl-C."},
					"wait_ms": {Type: "number", Description: "How long to wait for output, default 2000, max 60000."},
				},
				Required: []string{"id", "input"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				ID     int     `json:"id"`
				Input  string  `json:"input"`
				WaitMs float64 `json:"wait_ms"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			t, err := lookupTerminal(a.ID)
			if err != nil {
				return "", err
			}
			t.sendMu.Lock()
			defer t.sendMu.Unlock()
			if t.ended() {
				// A dead session is not a Go error: the three facts plus the
				// tail nobody drained are more useful to the model than "no
				// such terminal", which reads as if it typed the wrong id.
				return t.header(waitSessionExit) + "\n" + t.take() + "\n(this session has ended — open a new one with terminal_open)", nil
			}
			input := a.Input
			if !strings.HasSuffix(input, "\n") {
				input += "\n"
			}
			if _, err := t.ptmx.Write([]byte(input)); err != nil {
				return "", fmt.Errorf("terminal %d: %w", a.ID, err)
			}
			reason := t.wait(terminalWait(a.WaitMs))
			return t.header(reason) + "\n" + t.take(), nil
		},
	}
}

// TerminalReadTool drains whatever a session printed since the last read.
func TerminalReadTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "terminal_read",
			Description: "Read the output an open terminal session has produced since the last read, without typing anything and without waiting. Use it after a terminal_send that came back with wait_reason=timeout.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"id": {Type: "number", Description: "Session id returned by terminal_open (see terminal_list)."},
				},
				Required: []string{"id"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			t, err := lookupTerminal(a.ID)
			if err != nil {
				return "", err
			}
			// No wait_reason: nothing was waited on.
			return t.header("") + "\n" + t.take(), nil
		},
	}
}

// TerminalListTool lists the live sessions.
func TerminalListTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "terminal_list",
			Description: "List the open terminal sessions with their ids, statuses and labels. Sessions also appear in job_list, since terminal ids share the background-job id space.",
			Parameters: Schema{
				Type:       "object",
				Properties: map[string]Property{},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			termMu.Lock()
			live := make([]*terminal, 0, len(terminals))
			for _, t := range terminals {
				if t.alive() {
					live = append(live, t)
				}
			}
			termMu.Unlock()
			if len(live) == 0 {
				return "no open terminal sessions", nil
			}
			sort.Slice(live, func(i, j int) bool { return live[i].id < live[j].id })
			var b strings.Builder
			for _, t := range live {
				fmt.Fprintf(&b, "terminal %d: %s — %s\n", t.id, t.statusLine(), t.label())
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

// TerminalCloseTool terminates a session and reaps its child.
func TerminalCloseTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "terminal_close",
			Description: "Close a terminal session: its process group is killed and reaped before this returns. Close sessions you are done with — an open session holds a live shell.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"id": {Type: "number", Description: "Session id returned by terminal_open (see terminal_list)."},
				},
				Required: []string{"id"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			t, err := lookupTerminal(a.ID)
			if err != nil {
				return "", err
			}
			t.close()
			return fmt.Sprintf("terminal %d closed: %s", t.id, t.statusLine()), nil
		},
	}
}
