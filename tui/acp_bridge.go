package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/internal/headless"
	"github.com/javanhut/ollama_code/internal/instructions"
	"github.com/javanhut/ollama_code/internal/memory"
	"github.com/javanhut/ollama_code/tools"
)

// ACPRunner runs one agent turn for the ACP adapter (internal/acp). It does
// the same setup as RunHeadless — config, tool confinement, model/host
// resolution, registry, project instructions — but per workspace and with
// caller-supplied agent.Options hooks (streaming, permission prompts) and a
// seeded conversation history. The workspace root and process cwd must already
// point at cwd (the adapter serializes turns and sets both).
type ACPRunner func(ctx context.Context, cwd, prompt string, history []api.Message, mod func(*agent.Options)) (agent.Result, error)

// NewACPRunner builds the runner. model overrides the configured default
// (accepts provider:model); maxSteps 0 means the configured value.
func NewACPRunner(model string, maxSteps int) ACPRunner {
	var once sync.Once
	return func(ctx context.Context, cwd, prompt string, history []api.Message, mod func(*agent.Options)) (agent.Result, error) {
		cfg := loadConfig()
		once.Do(func() {
			tools.SetJailAllowlist(cfg.JailAllowlist)
			if cfg.ShellSandbox != nil {
				tools.SetShellSandboxEnabled(*cfg.ShellSandbox)
			}
			tools.SetShellNetworkAllowed(cfg.ShellNetwork)
			tools.SetShellNetworkAllowlist(cfg.ShellNetworkAllow)
			if cfg.Format != nil {
				tools.SetFormatEnabled(*cfg.Format)
			}
		})
		tools.SetWorkspaceRoot(cwd)
		tools.ConfigureLSP(cfg.LSP == nil || *cfg.LSP, workspaceRoot(), cfg.LSPServers)

		spec := strings.TrimSpace(model)
		if spec == "" {
			spec = strings.TrimSpace(cfg.Model)
		}
		if spec == "" {
			return agent.Result{}, errors.New("no model selected: pass --model to `ocode acp`, or set a default with /model use in the TUI")
		}
		host, m := hostForSpec(cfg, spec)

		notes := &sessionNotes{}
		notes.load()
		mem, _ := memory.New(memoryPath(workspaceRoot()))
		registry := baseRegistry(notes, &todoList{}, mem)

		steps := maxSteps
		if steps <= 0 {
			steps = maxStepsFromConfig(cfg)
		}
		opts := agent.Options{
			Model:       m,
			System:      headless.DefaultSystem + "\n\nThe user is reading your output live in their editor, and approves file changes and commands as you request them. If a call is declined, do not retry it; continue without it or explain.",
			MaxSteps:    steps,
			Permissions: cfg.Permissions,
			NumCtx:      headlessNumCtx(cfg, host, spec, m),
			Verify:      cfg.Verify == nil || *cfg.Verify,
			VerifyCmd:   cfg.VerifyCmd,
		}
		if len(history) > 0 {
			opts.PriorMessages = history
		}
		if cfg.ProjectInstructions == nil || *cfg.ProjectInstructions {
			wd, _ := os.Getwd()
			set := instructions.Load(instructions.Options{Cwd: wd, Extra: cfg.Instructions})
			opts.System += set.Render()
			tracker := instructions.NewTracker(wd, set)
			opts.AugmentResult = func(call tools.ToolCall, result string) string {
				return tools.AppendEvidence(result, tracker.ForPath(headlessPathArg(call)))
			}
		}
		if mod != nil {
			mod(&opts)
		}
		return agent.Run(ctx, host, registry, prompt, opts)
	}
}

// AppVersion is the build-stamped version, reported to ACP clients.
func AppVersion() string { return appVersion }
