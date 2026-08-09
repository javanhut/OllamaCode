package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/javanhut/ollama_code/tui"
)

// cliFlags holds the command line. With no -p the TUI starts as before; -p
// runs a single non-interactive agent turn (see docs/commands.md, "Headless
// mode").
type cliFlags struct {
	prompt   string
	model    string
	json     bool
	maxSteps int
}

// parseFlags parses args (excluding argv[0]). -p and --prompt are the same
// flag: Go's flag package treats one and two dashes alike, and both names are
// registered onto one variable so either spelling works.
func parseFlags(args []string, stderr io.Writer) (cliFlags, error) {
	var f cliFlags
	fs := flag.NewFlagSet("ocode", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.prompt, "p", "", "run one prompt non-interactively and print the final answer (shorthand for -prompt)")
	fs.StringVar(&f.prompt, "prompt", "", "run one prompt non-interactively and print the final answer")
	fs.StringVar(&f.model, "model", "", "model for the headless run (default: configured model); accepts provider:model")
	fs.BoolVar(&f.json, "json", false, "with -p, emit a single JSON object instead of plain text")
	fs.IntVar(&f.maxSteps, "max-steps", 0, "with -p, cap tool-call rounds (default: configured max_steps)")
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, err
	}
	return f, nil
}

func main() {
	f, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	if f.prompt == "" {
		if err := tui.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	err = tui.RunHeadless(context.Background(), tui.HeadlessOptions{
		Prompt:   f.prompt,
		Model:    f.model,
		MaxSteps: f.maxSteps,
		JSON:     f.json,
	}, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
