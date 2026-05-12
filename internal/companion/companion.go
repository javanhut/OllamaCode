// Package companion is the CLI-side client that manages the ollama-companion
// subprocess. The companion is a separate Gio-based GUI binary that captures
// the microphone for STT and plays back assistant replies via TTS.
//
// Communication is line-delimited JSON over the child's stdin/stdout, using
// the message envelope defined in github.com/javanhut/ollama_code/companion.
package companion

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// BinaryName is the filename we look for on disk / in PATH.
const BinaryName = "ollama-companion"

// Transcript is what the GUI hands back when STT has detected an utterance.
type Transcript struct {
	Text string
}

// Client wraps the running companion subprocess.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	Transcripts <-chan Transcript
	Errors      <-chan error

	closeOnce sync.Once
}

// Start spawns the companion binary. The location is resolved in this order:
//  1. $OLLAMA_COMPANION_BIN if set
//  2. ollama-companion next to the current executable
//  3. ollama-companion on $PATH
//
// Returns a Client with two channels: Transcripts (STT results, appended to
// the input box by the caller) and Errors (non-fatal warnings to surface).
func Start() (*Client, error) {
	binPath, err := resolveBinary()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(binPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", binPath, err)
	}

	tCh := make(chan Transcript, 8)
	eCh := make(chan error, 4)

	c := &Client{
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		Transcripts: tCh,
		Errors:      eCh,
	}

	go c.readStdout(tCh, eCh)
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	return c, nil
}

func (c *Client) readStdout(tCh chan<- Transcript, eCh chan<- error) {
	defer close(tCh)
	defer close(eCh)

	sc := bufio.NewScanner(c.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "transcript":
			if msg.Text != "" {
				select {
				case tCh <- Transcript{Text: msg.Text}:
				default:
				}
			}
		case "error":
			select {
			case eCh <- errors.New(msg.Text):
			default:
			}
		}
	}
}

// Speak asks the companion to vocalize text via TTS. Non-blocking; returns an
// error only if the pipe is broken. Text is sanitized so fenced code blocks
// aren't read aloud verbatim.
func (c *Client) Speak(text string) error {
	cleaned := SanitizeForSpeech(text)
	if cleaned == "" {
		return nil
	}
	return c.send(message{Type: "speak", Text: cleaned})
}

var (
	fencedBlockRE  = regexp.MustCompile("(?s)```.*?```")
	inlineCodeRE   = regexp.MustCompile("`([^`\n]+)`")
	imageRE        = regexp.MustCompile(`!\[[^\]]*\]\([^)]+\)`)
	linkRE         = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	autolinkRE     = regexp.MustCompile(`<https?://[^>]+>`)
	asteriskWrapRE = regexp.MustCompile(`\*+([^*\n]+?)\*+`)
	strikeRE       = regexp.MustCompile(`~~([^~\n]+)~~`)
	headingRE      = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s+`)
	blockquoteRE   = regexp.MustCompile(`(?m)^\s{0,3}>\s*`)
	listBulletRE   = regexp.MustCompile(`(?m)^\s{0,3}[-*+]\s+`)
	listNumberRE   = regexp.MustCompile(`(?m)^\s{0,3}\d+\.\s+`)
	hrRE           = regexp.MustCompile(`(?m)^\s{0,3}(?:-\s*){3,}$|^\s{0,3}(?:\*\s*){3,}$|^\s{0,3}(?:_\s*){3,}$`)
	tablePipeRE    = regexp.MustCompile(`\|`)
	extraSpaceRE   = regexp.MustCompile(`[ \t]{2,}`)
	extraBlankRE   = regexp.MustCompile(`\n{3,}`)
)

// SanitizeForSpeech strips markdown noise that reads poorly when vocalized
// — fenced code blocks, inline code, links/images, bold/italic asterisks,
// headings, list markers, blockquotes, strikethroughs, table pipes, and
// horizontal rules. Underscores are left alone (commonly appear in
// identifiers, file paths, URLs in prose).
func SanitizeForSpeech(s string) string {
	// Block-level structure first.
	s = fencedBlockRE.ReplaceAllString(s, " (code block omitted) ")
	s = imageRE.ReplaceAllString(s, "")
	s = linkRE.ReplaceAllString(s, "$1")
	s = autolinkRE.ReplaceAllStringFunc(s, func(m string) string {
		// <https://...> -> drop entirely; URLs don't read well
		return ""
	})

	// Line-anchored markers.
	s = headingRE.ReplaceAllString(s, "")
	s = blockquoteRE.ReplaceAllString(s, "")
	s = listBulletRE.ReplaceAllString(s, "")
	s = listNumberRE.ReplaceAllString(s, "")
	s = hrRE.ReplaceAllString(s, "")

	// Inline emphasis.
	s = inlineCodeRE.ReplaceAllString(s, "$1")
	s = asteriskWrapRE.ReplaceAllString(s, "$1")
	s = strikeRE.ReplaceAllString(s, "$1")

	// Tables: drop pipes; surrounding whitespace gets collapsed below.
	s = tablePipeRE.ReplaceAllString(s, " ")

	// Catch-all for any orphan asterisks left behind by mismatched markdown.
	s = strings.ReplaceAll(s, "*", "")
	s = strings.ReplaceAll(s, "`", "")

	// Whitespace cleanup so piper doesn't insert weird pauses.
	s = extraSpaceRE.ReplaceAllString(s, " ")
	s = extraBlankRE.ReplaceAllString(s, "\n\n")

	return strings.TrimSpace(s)
}

// Stop cancels any in-flight TTS playback.
func (c *Client) Stop() error {
	return c.send(message{Type: "stop"})
}

func (c *Client) send(m message) error {
	b, err := json.Marshal(&m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

// Close shuts the companion down cleanly.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		// Best-effort polite exit, then force-kill.
		_ = c.send(message{Type: "shutdown"})
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		err = c.cmd.Wait()
	})
	return err
}

type message struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func resolveBinary() (string, error) {
	if env := os.Getenv("OLLAMA_COMPANION_BIN"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env, nil
		}
		return "", fmt.Errorf("OLLAMA_COMPANION_BIN points at %q which doesn't exist", env)
	}

	var tried []string

	// Sibling of the running ollama-code binary. With `go run` /
	// `make dev`, os.Executable() returns the temp build path so this
	// will usually miss — fine, we keep looking.
	if exe, err := os.Executable(); err == nil {
		guess := filepath.Join(filepath.Dir(exe), BinaryName)
		if _, err := os.Stat(guess); err == nil {
			return guess, nil
		}
		tried = append(tried, guess)
	}

	// Current working directory — common when you ran `make build-companion`
	// in the repo and launch `./ollama-code` from the same place.
	if cwd, err := os.Getwd(); err == nil {
		guess := filepath.Join(cwd, BinaryName)
		if _, err := os.Stat(guess); err == nil {
			return guess, nil
		}
		tried = append(tried, guess)
	}

	// The repo root, even if you launched from a subdirectory.
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				guess := filepath.Join(dir, BinaryName)
				if _, err := os.Stat(guess); err == nil {
					return guess, nil
				}
				tried = append(tried, guess)
				break
			}
		}
	}

	if p, err := exec.LookPath(BinaryName); err == nil {
		return p, nil
	}

	return "", fmt.Errorf("%s not found (looked in: %s) — set $OLLAMA_COMPANION_BIN or run `make build-companion`",
		BinaryName, strings.Join(tried, ", "))
}
