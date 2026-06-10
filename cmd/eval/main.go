// Command eval is a small, self-contained benchmark for OllamaCode's agent loop.
// It runs a set of scripted coding tasks through the headless agent (internal/
// agent) against a configured Ollama model and reports pass/fail per task, so
// you can compare models and catch regressions in the tool/loop layer.
//
// Usage:
//
//	go run ./cmd/eval -model qwen2.5-coder:7b
//	go run ./cmd/eval -model deepseek-coder-v2 -host http://localhost:11434
//
// It requires a running Ollama with the named model pulled.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/mcp"
)

const evalSystem = `You are an automated coding agent being evaluated. Use the available tools to complete the task in the current working directory, then stop. Be efficient and do not ask questions.`

type task struct {
	name   string
	prompt string
	setup  map[string]string                       // relative path -> contents
	check  func(dir, output string) (bool, string) // success criterion
}

func tasks() []task {
	return []task{
		{
			name:   "create-file",
			prompt: "Create a file named hello.txt in the current directory whose entire contents are exactly: Hello, World!",
			check: func(dir, _ string) (bool, string) {
				b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
				if err != nil {
					return false, "hello.txt not created"
				}
				if strings.TrimSpace(string(b)) != "Hello, World!" {
					return false, fmt.Sprintf("unexpected contents: %q", string(b))
				}
				return true, "file created with correct contents"
			},
		},
		{
			name: "fix-bug",
			setup: map[string]string{
				"calc.go": "package calc\n\n// Add returns the sum of a and b.\nfunc Add(a, b int) int {\n\treturn a + a\n}\n",
			},
			prompt: "The Add function in calc.go has a bug: it returns a + a instead of the sum of its two parameters. Fix it so it returns the sum of a and b.",
			check: func(dir, _ string) (bool, string) {
				b, err := os.ReadFile(filepath.Join(dir, "calc.go"))
				if err != nil {
					return false, "calc.go missing"
				}
				s := string(b)
				if strings.Contains(s, "a + a") {
					return false, "bug still present (a + a)"
				}
				if strings.Contains(s, "a + b") || strings.Contains(s, "b + a") {
					return true, "bug fixed"
				}
				return false, "did not find a corrected sum expression"
			},
		},
		{
			name: "investigate",
			setup: map[string]string{
				"a.go":      "package x\n",
				"b.go":      "package x\n",
				"notes.txt": "not go\n",
			},
			prompt: "How many files ending in .go are in the current directory (non-recursive)? Respond with ONLY the number.",
			check: func(_, output string) (bool, string) {
				if strings.Contains(output, "2") {
					return true, "answered correctly"
				}
				return false, fmt.Sprintf("answer was: %q", strings.TrimSpace(output))
			},
		},
	}
}

func main() {
	model := flag.String("model", "", "model to evaluate (required)")
	host := flag.String("host", "http://localhost:11434", "backend host URL")
	provider := flag.String("provider", api.ProviderOllama, "backend: ollama or mlx")
	steps := flag.Int("steps", 15, "max agent steps per task")
	flag.Parse()
	if *model == "" {
		fmt.Fprintln(os.Stderr, "usage: eval -model <name> [-host url] [-provider ollama|mlx] [-steps n]")
		os.Exit(2)
	}

	h := api.NewProvider(*provider, *host)
	reg := mcp.DefaultRegistry()

	origWd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "getwd:", err)
		os.Exit(1)
	}

	all := tasks()
	pass := 0
	fmt.Printf("Evaluating model %q against %d tasks\n\n", *model, len(all))
	for _, t := range all {
		dir, err := os.MkdirTemp("", "ollamacode-eval-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "mkdtemp:", err)
			os.Exit(1)
		}
		for rel, content := range t.setup {
			p := filepath.Join(dir, rel)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			_ = os.WriteFile(p, []byte(content), 0o644)
		}

		_ = os.Chdir(dir)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		start := time.Now()
		res, runErr := agent.Run(ctx, h, reg, t.prompt, agent.Options{
			Model:    *model,
			System:   evalSystem,
			MaxSteps: *steps,
		})
		cancel()
		_ = os.Chdir(origWd)

		ok, detail := false, ""
		if runErr != nil {
			detail = "agent error: " + runErr.Error()
		} else {
			ok, detail = t.check(dir, res.Output)
		}
		status := "FAIL"
		if ok {
			status = "PASS"
			pass++
		}
		fmt.Printf("[%s] %-12s steps=%-2d %4dms — %s\n", status, t.name, res.Steps, time.Since(start).Milliseconds(), detail)
		_ = os.RemoveAll(dir)
	}

	fmt.Printf("\n%d/%d passed\n", pass, len(all))
	if pass < len(all) {
		os.Exit(1)
	}
}
