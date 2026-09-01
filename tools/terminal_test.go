//go:build unix

package tools

import (
	"bytes"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
)

// openTestTerminal opens a session through the tool handler (so the jail check
// and the sandbox wrap are exercised) and returns its id and pid.
func openTestTerminal(t *testing.T, command string) (int, int) {
	t.Helper()
	out, err := callTool(t, TerminalOpenTool(), fmt.Sprintf(`{"command":%q}`, command))
	if err != nil {
		t.Fatalf("terminal_open: %v", err)
	}
	// A missing-sandbox notice can precede the result line, so scan lines.
	for _, line := range strings.Split(out, "\n") {
		var id, pid int
		if _, err := fmt.Sscanf(line, "terminal %d opened (pid %d)", &id, &pid); err == nil {
			return id, pid
		}
	}
	t.Fatalf("no session id in terminal_open result %q", out)
	return 0, 0
}

// hasLine reports whether s contains a line that is exactly want. Contains is
// not good enough here: the pty echoes the typed command back, so a substring
// check passes on the echo alone even when the program produced nothing.
func hasLine(s, want string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func TestTerminalSendSilenceWhileAlive(t *testing.T) {
	t.Cleanup(CloseTerminals)
	id, _ := openTestTerminal(t, "sh")

	out, err := callTool(t, TerminalSendTool(), fmt.Sprintf(`{"id":%d,"input":"echo hi"}`, id))
	if err != nil {
		t.Fatalf("terminal_send: %v", err)
	}
	if !strings.Contains(out, "wait_reason="+waitSilence) {
		t.Errorf("expected wait_reason=%s, got %q", waitSilence, out)
	}
	if !strings.Contains(out, "alive=true") {
		t.Errorf("a shell at its prompt is still alive, got %q", out)
	}
	if !hasLine(out, "hi") {
		t.Errorf("expected a line that is exactly \"hi\" (not just the echoed command), got %q", out)
	}
}

func TestTerminalSendSessionExit(t *testing.T) {
	t.Cleanup(CloseTerminals)
	id, _ := openTestTerminal(t, "sh")

	out, err := callTool(t, TerminalSendTool(), fmt.Sprintf(`{"id":%d,"input":"exit"}`, id))
	if err != nil {
		t.Fatalf("terminal_send: %v", err)
	}
	if !strings.Contains(out, "wait_reason="+waitSessionExit) {
		t.Errorf("expected wait_reason=%s, got %q", waitSessionExit, out)
	}
	if !strings.Contains(out, "alive=false") {
		t.Errorf("expected alive=false after the shell exited, got %q", out)
	}
	// The three facts are reported together; exit_code appearing at all also
	// pins that close(exited) lands after the exit status is recorded.
	if !strings.Contains(out, "exit_code=") {
		t.Errorf("expected an exit_code field, got %q", out)
	}
}

func TestTerminalSendTimeoutStaysAlive(t *testing.T) {
	t.Cleanup(CloseTerminals)
	id, _ := openTestTerminal(t, "sh")

	// wait_ms must be well below terminalQuiet: the pty echoes the typed line
	// back immediately, so sawOutput is set and the quiet timer starts running
	// — only a deadline shorter than terminalQuiet makes this deterministic.
	out, err := callTool(t, TerminalSendTool(), fmt.Sprintf(`{"id":%d,"input":"sleep 2","wait_ms":100}`, id))
	if err != nil {
		t.Fatalf("terminal_send: %v", err)
	}
	// The regression the whole design exists to prevent: a command still
	// working must not read as silence ("done, printed nothing") or as
	// session_exit ("the shell died"). Removing the sawOutput guard from
	// wait()'s quiet branch fails this.
	if !strings.Contains(out, "wait_reason="+waitTimeout) {
		t.Errorf("expected wait_reason=%s while the command is still running, got %q", waitTimeout, out)
	}
	if !strings.Contains(out, "alive=true") {
		t.Errorf("a busy shell is still alive, got %q", out)
	}
}

// A shell that exits leaving a background job behind is the case that separates
// "the child was reaped" from "the pty went quiet": the orphan keeps the slave
// tty open, so the master read does not return (linux; darwin revokes the tty
// on session-leader exit and hides this). Reaping behind that read left the
// shell a zombie, alive() answering true forever, and eight such sessions
// wedging terminal_open.
func TestTerminalExitWithBackgroundChildStillReaps(t *testing.T) {
	t.Cleanup(CloseTerminals)
	id, pid := openTestTerminal(t, "sh")

	for _, input := range []string{"sleep 5 &", "exit"} {
		if _, err := callTool(t, TerminalSendTool(), fmt.Sprintf(`{"id":%d,"input":%q}`, id, input)); err != nil {
			t.Fatalf("terminal_send %q: %v", input, err)
		}
	}

	term, err := lookupTerminal(id)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-term.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the shell exited but the session never reaped it")
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("pid %d is still unreaped after the shell exited (kill 0 → %v)", pid, err)
	}
	out, err := callTool(t, TerminalListTool(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no open terminal sessions") {
		t.Errorf("a reaped session must not still list as alive, got %q", out)
	}
}

func TestTerminalOpenRejectsWorkingDirOutsideJail(t *testing.T) {
	t.Cleanup(CloseTerminals)
	_, err := callTool(t, TerminalOpenTool(), `{"command":"sh","working_dir":"/etc"}`)
	if err == nil {
		t.Fatal("expected working_dir outside the workspace to be rejected")
	}
	if !strings.Contains(err.Error(), "workspace root") {
		t.Errorf("expected a jail error naming the workspace root, got %v", err)
	}
	// Rejected before anything was spawned, not after.
	out, err := callTool(t, TerminalListTool(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no open terminal sessions") {
		t.Errorf("a rejected open must leave no session behind, got %q", out)
	}
}

func TestTerminalCloseReapsChild(t *testing.T) {
	t.Cleanup(CloseTerminals)
	id, pid := openTestTerminal(t, "sh")

	if _, err := callTool(t, TerminalCloseTool(), fmt.Sprintf(`{"id":%d}`, id)); err != nil {
		t.Fatalf("terminal_close: %v", err)
	}
	// Signal 0 probes for existence. close() must have reaped the child, not
	// merely signalled it: a zombie or a live process here is exactly the
	// "dispose must reach quiescence" failure.
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("pid %d still exists after terminal_close (kill 0 → %v)", pid, err)
	}

	done := make(chan struct{})
	go func() {
		_, _ = callTool(t, TerminalCloseTool(), fmt.Sprintf(`{"id":%d}`, id))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second terminal_close hung: close must be idempotent on an already-reaped session")
	}
}

func TestTerminalBufferDropsOldestBytes(t *testing.T) {
	term := &terminal{wrote: make(chan struct{}, 1), exited: make(chan struct{})}
	term.append(bytes.Repeat([]byte("a"), terminalBufferMax))
	term.append([]byte("TAIL"))

	out := term.take()
	if n := strings.Count(out, terminalDroppedNotice); n != 1 {
		t.Fatalf("expected the dropped-bytes notice exactly once, got %d in %d bytes", n, len(out))
	}
	body := strings.TrimPrefix(out, terminalDroppedNotice+"\n")
	if len(body) != terminalBufferMax {
		t.Errorf("buffer should hold exactly %d bytes after the trim, got %d", terminalBufferMax, len(body))
	}
	if !strings.HasSuffix(body, "TAIL") {
		t.Error("the newest bytes must survive the trim")
	}
	if n := strings.Count(body, "a"); n != terminalBufferMax-len("TAIL") {
		t.Errorf("expected %d oldest bytes to be dropped, %d remain", len("TAIL"), n)
	}
	if again := term.take(); again != "" {
		t.Errorf("take must drain: second read returned %q", again)
	}
}
