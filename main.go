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
	fs.BoolVar(&f.json, "json", false, "with -p, emit a single JSON object instead of plain text")
	fs.IntVar(&f.maxSteps, "max-steps", 0, "with -p, cap tool-call rounds (default: configured max_steps)")
	fs.BoolVar(&f.debug, "debug", false, "write a fresh redacted model/tool trace to ./ocode.log")
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, err
	}
	if f.resumeSet && f.prompt != "" {
		return cliFlags{}, fmt.Errorf("--resume cannot be combined with -p (headless runs start fresh)")
	}
	return f, nil
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
		Prompt:    f.prompt,
		Model:     f.model,
		MaxSteps:  f.maxSteps,
		JSON:      f.json,
		DebugPath: debugPath,
	}, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
