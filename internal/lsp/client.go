// Package lsp is a minimal Language Server Protocol client: enough to ask a
// real language server where a symbol is defined, who references it, what it
// is, and what is wrong with a file. It exists because the grep-and-regex
// codeintel path can only guess, and guesses about Python or Rust definitions
// are exactly the kind of confident-but-wrong context a small model cannot
// recover from.
//
// Scope is deliberately narrow. No incremental sync, no workspace edits, no
// completion: files are opened whole, queried, and left open. Everything here
// degrades to "no answer" rather than to an error, because every caller has a
// working fallback and a missing language server must never be fatal.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// maxHeaderBytes bounds one message header. A server that streams megabytes
// without a blank line is broken, not verbose.
const maxHeaderBytes = 1 << 16

// maxMessageBytes bounds one message body, so a runaway server cannot exhaust
// memory here.
const maxMessageBytes = 32 << 20

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Params json.RawMessage `json:"params"`
	Error  *responseError  `json:"error"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *responseError) Error() string { return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message) }

// Client is one running language server process.
type Client struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[int]chan response

	diagMu sync.RWMutex
	diags  map[string][]Diagnostic

	closeOnce sync.Once
	done      chan struct{}
	// exitErr records why the reader loop stopped, so a call that races a
	// crashing server reports the crash instead of blocking to its deadline.
	exitErr error
}

// dial starts the server binary and begins reading its output. The caller owns
// the returned client and must Close it.
func dial(ctx context.Context, bin string, args []string, dir string) (*Client, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// A language server's stderr is its own diagnostics channel, not ours.
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		pending: map[int]chan response{},
		diags:   map[string][]Diagnostic{},
		done:    make(chan struct{}),
	}
	go c.read(stdout)
	return c, nil
}

// read consumes framed messages until the stream ends, routing responses to
// their waiting caller and diagnostics into the client's map.
func (c *Client) read(r io.Reader) {
	defer func() {
		// A panic in the reader must not take the process down with it: the
		// worst honest outcome is that this server stops answering.
		if p := recover(); p != nil && c.exitErr == nil {
			c.exitErr = fmt.Errorf("language server reader panicked: %v", p)
		}
		c.shutdownPending()
	}()
	br := bufio.NewReader(r)
	for {
		length, err := readHeader(br)
		if err != nil {
			c.exitErr = err
			return
		}
		if length <= 0 || length > maxMessageBytes {
			c.exitErr = fmt.Errorf("language server sent an implausible message length %d", length)
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(br, body); err != nil {
			c.exitErr = err
			return
		}
		var msg response
		if err := json.Unmarshal(body, &msg); err != nil {
			continue // a message we cannot parse is one we did not need
		}
		if msg.Method == "textDocument/publishDiagnostics" {
			c.storeDiagnostics(msg.Params)
			continue
		}
		if msg.ID == nil {
			continue // a notification or a server request we do not implement
		}
		c.mu.Lock()
		ch, ok := c.pending[*msg.ID]
		delete(c.pending, *msg.ID)
		c.mu.Unlock()
		if ok {
			ch <- msg
		}
	}
}

// shutdownPending releases every in-flight caller when the server goes away,
// so a crash surfaces as an error rather than as a hang until the deadline.
func (c *Client) shutdownPending() {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[int]chan response{}
	c.mu.Unlock()
	for id, ch := range pending {
		ch <- response{ID: &id, Error: &responseError{Message: "language server exited"}}
	}
	c.closeOnce.Do(func() { close(c.done) })
}

func readHeader(br *bufio.Reader) (int, error) {
	length := -1
	read := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		read += len(line)
		if read > maxHeaderBytes {
			return 0, errors.New("language server header is implausibly long")
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if length < 0 {
				return 0, errors.New("language server sent a message with no Content-Length")
			}
			return length, nil
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "content-length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, fmt.Errorf("bad Content-Length: %w", err)
		}
		length = n
	}
}

func (c *Client) write(payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.stdin.Write(body)
	return err
}

// notify sends a request that expects no reply.
func (c *Client) notify(method string, params any) error {
	return c.write(request{JSONRPC: "2.0", Method: method, Params: params})
}

// call sends a request and waits for its reply, the caller's context, or the
// server's death — whichever comes first.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case msg := <-ch:
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	}
}

// Close shuts the server down politely, then kills it. Politeness has a short
// budget: a wedged server must not delay the caller's exit.
func (c *Client) Close() {
	if c == nil {
		return
	}
	_ = c.notify("exit", nil)
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	c.closeOnce.Do(func() { close(c.done) })
}
