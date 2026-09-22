package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/javanhut/ollama_code/tools"
	"github.com/javanhut/ollama_code/tui"
)

// cliFlags holds the command line. With no -p the TUI starts as before; -p
// runs a single non-interactive agent turn (see docs/commands.md, "Headless
// mode"). --resume restores a saved session on startup (TUI only).
type cliFlags struct {
	prompt    string
	model     string
	json      bool
	format    string // --output-format: text | json | stream-json
	maxSteps  int
	debug     bool
	resume    string // session name; "" with resumeSet = latest auto-save
	resumeSet bool
}

// extractResume handles --resume before flag parsing: its value is optional
// (bare --resume means "the last auto-saved session"), which flag.StringVar
// cannot express. Both -resume/--resume and the =value form are accepted; a
// separate token is consumed as the name only when it doesn't look like
// another flag.
func extractResume(args []string) (rest []string, id string, set bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-resume" || a == "--resume" {
			set = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				id = args[i+1]
				i++
			}
			continue
		}
		if v, ok := strings.CutPrefix(a, "-resume="); ok {
			id, set = v, true
			continue
		}
		if v, ok := strings.CutPrefix(a, "--resume="); ok {
			id, set = v, true
			continue
		}
		rest = append(rest, a)
	}
	return rest, id, set
}

// parseFlags parses args (excluding argv[0]). -p and --prompt are the same
// flag: Go's flag package treats one and two dashes alike, and both names are
// registered onto one variable so either spelling works.
func parseFlags(args []string, stderr io.Writer) (cliFlags, error) {
	var f cliFlags
	args, f.resume, f.resumeSet = extractResume(args)
	fs := flag.NewFlagSet("ocode", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ocode [--resume [name]] [flags]")
		fmt.Fprintln(stderr, "  --resume [name]  restore the last auto-saved session, or a named /save session")
		fs.PrintDefaults()
	}
	fs.StringVar(&f.prompt, "p", "", "run one prompt non-interactively and print the final answer (shorthand for -prompt)")
	fs.StringVar(&f.prompt, "prompt", "", "run one prompt non-interactively and print the final answer")
	fs.StringVar(&f.model, "model", "", "model for the headless run (default: configured model); accepts provider:model")
	fs.BoolVar(&f.json, "json", false, "with -p, emit a single JSON object instead of plain text (same as --output-format json)")
	fs.StringVar(&f.format, "output-format", "", "with -p: text (default), json (one object), or stream-json (newline-delimited events)")
	fs.IntVar(&f.maxSteps, "max-steps", 0, "with -p, cap tool-call rounds (default: configured max_steps)")
	fs.BoolVar(&f.debug, "debug", false, "write a fresh redacted model/tool trace to ./ocode.log")
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, err // flag already printed it
	}
	f, err := validateFlags(f)
	if err != nil {
		// main exits 2 without printing, so say why here.
		fmt.Fprintln(stderr, "error:", err)
	}
	return f, err
}

// validateFlags checks combinations the flag package cannot, and resolves the
// -json alias into the output format.
func validateFlags(f cliFlags) (cliFlags, error) {
	if f.resumeSet && f.prompt != "" {
		return cliFlags{}, fmt.Errorf("--resume cannot be combined with -p (headless runs start fresh)")
	}
	switch f.format {
	case "":
		if f.json {
			f.format = "json"
		}
	case "text", "json", "stream-json":
		if f.json && f.format != "json" {
			return cliFlags{}, fmt.Errorf("-json conflicts with --output-format %s", f.format)
		}
	default:
		return cliFlags{}, fmt.Errorf("--output-format must be text, json, or stream-json (got %q)", f.format)
	}
	return f, nil
}

// maxStdinPrompt caps piped input. A prompt is sent to the model on every
// step, so anything near this size would not fit a local model's context
// anyway; failing loudly beats silently truncating the user's input.
const maxStdinPrompt = 1 << 20

// mergeStdinPrompt folds piped stdin into the headless prompt, the way
// opencode's `run` does: with -p, stdin is appended after a blank line (so
// `git diff | ocode -p "review this"` works); without -p, stdin IS the prompt.
// An empty or whitespace-only stdin leaves the prompt unchanged.
func mergeStdinPrompt(prompt string, stdin io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(stdin, maxStdinPrompt+1))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	if len(data) > maxStdinPrompt {
		return "", fmt.Errorf("piped stdin exceeds %d bytes; pass a file path in the prompt and let the model read it instead", maxStdinPrompt)
	}
	piped := strings.TrimRight(string(data), "\n\r\t ")
	if strings.TrimSpace(piped) == "" {
		return prompt, nil
	}
	if strings.TrimSpace(prompt) == "" {
		return piped, nil
	}
	return prompt + "\n\n" + piped, nil
}

// stdinPiped reports whether stdin is a pipe or file rather than a terminal.
func stdinPiped() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice == 0
}

func main() {
	f, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	// Oversized tool output spills to a private temp file; it is plaintext of
	// whatever the model read, so it should not outlive the session on an
	// ordinary exit. The os.Exit error paths below skip this, as does a crash —
	// that is what the budget cap and the OS reaper are for.
	defer tools.CleanupSpills()
	// Language servers are child processes; without this they outlive the
	// session that started them.
	defer tools.CloseLSP()
	// Terminal sessions are child shells; closing awaits each child's exit
	// rather than returning once the signal is sent, so nothing is left running.
	defer tools.CloseTerminals()
	debugPath := ""
	if f.debug {
		debugPath, err = filepath.Abs("ocode.log")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: resolve debug log:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "debug log:", debugPath)
	}
	// Piped input only means a headless run when nothing asked for the TUI
	// explicitly: --resume is interactive, and bubbletea needs a real terminal.
	if !f.resumeSet && stdinPiped() {
		f.prompt, err = mergeStdinPrompt(f.prompt, os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	if f.prompt == "" {
		if f.resumeSet {
			if err := tui.RunResumeWithDebug(f.resume, debugPath); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
		if err := tui.RunWithDebug(debugPath); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	err = tui.RunHeadless(context.Background(), tui.HeadlessOptions{
		Prompt:       f.prompt,
		Model:        f.model,
		MaxSteps:     f.maxSteps,
		JSON:         f.json,
		OutputFormat: f.format,
		DebugPath:    debugPath,
	}, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
