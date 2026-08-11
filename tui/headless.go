package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/javanhut/ollama_code/internal/headless"
	"github.com/javanhut/ollama_code/internal/memory"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/tools"
)

// baseRegistry builds the model-independent part of the tool registry: the
// built-in tools plus session notes, todos, and workspace memory. The TUI adds
// its model-bound tools (mode switching, sub-agents, semantic search, MCP) on
// top; headless one-shot runs use this as-is.
func baseRegistry(notes *sessionNotes, todos *todoList, mem *memory.Store) *tools.Registry {
	r := tools.DefaultRegistry()
	r.Register(readNotesTool(notes))
	r.Register(updateNotesTool(notes))
	r.Register(appendNotesTool(notes))
	r.Register(todoWriteTool(todos))
	r.Register(todoReadTool(todos))
	r.Register(rememberTool(mem))
	r.Register(recallTool(mem))
	r.Register(forgetTool(mem))
	return r
}

// HeadlessOptions configures a one-shot, non-interactive run (ocode -p).
type HeadlessOptions struct {
	Prompt    string
	Model     string // overrides the configured default model; accepts provider:model
	MaxSteps  int    // tool-call rounds; 0 = configured max_steps, else the default
	JSON      bool   // emit a single JSON object instead of plain text
	DebugPath string // fresh redacted JSONL trace; empty disables debug logging
}

// RunHeadless executes one agent turn without the TUI and writes the final
// answer to stdout (plain text, or one JSON object with JSON set). Safety
// posture is identical to the TUI's confinement and to what cmd/eval already
// relies on: the tools layer jails every filesystem path to the workspace root
// (plus the configured allowlist) and wraps run_shell in the OS sandbox. What
// does not exist headless is the TUI's permission prompt, so a headless run is
// the equivalent of auto mode with approval pre-granted — confined to the
// workspace, but non-interactive by design.
func RunHeadless(ctx context.Context, opts HeadlessOptions, stdout io.Writer) (runErr error) {
	cfg := loadConfig()
	var recorder *tracepkg.Recorder
	if opts.DebugPath != "" {
		var err error
		recorder, err = tracepkg.OpenFresh(opts.DebugPath)
		if err != nil {
			return fmt.Errorf("open debug log %s: %w", opts.DebugPath, err)
		}
		cwd, _ := os.Getwd()
		_ = recorder.Record(tracepkg.Event{Kind: "session_start", Metadata: map[string]any{
			"surface": "headless", "working_directory": cwd, "format": "redacted-jsonl", "schema_version": 2,
		}})
		defer func() {
			metadata := map[string]any{"reason": "clean_exit"}
			if runErr != nil {
				metadata["reason"] = "error"
				metadata["error"] = runErr.Error()
			}
			_ = recorder.Record(tracepkg.Event{Kind: "session_end", Metadata: metadata})
			_ = recorder.Close()
		}()
	}
	// Same confinement pins as New(): order matters, tools must be jailed
	// before any of them can run.
	tools.SetWorkspaceRoot(workspaceRoot())
	tools.SetJailAllowlist(cfg.JailAllowlist)
	if cfg.ShellSandbox != nil {
		tools.SetShellSandboxEnabled(*cfg.ShellSandbox)
	}

	spec := strings.TrimSpace(opts.Model)
	if spec == "" {
		spec = strings.TrimSpace(cfg.Model)
	}
	if spec == "" {
		return errors.New("no model selected — pass -model, or set a default with /model use in the TUI")
	}
	host, model := hostForSpec(cfg, spec)

	notes := &sessionNotes{}
	notes.load()
	mem, _ := memory.New(memoryPath(workspaceRoot()))
	registry := baseRegistry(notes, &todoList{}, mem)

	maxSteps := opts.MaxSteps
	if maxSteps <= 0 {
		maxSteps = maxStepsFromConfig(cfg)
	}
	res, err := headless.Run(ctx, host, registry, opts.Prompt, headless.Options{
		Model:    model,
		MaxSteps: maxSteps,
		Trace:    recorder,
	})
	if err != nil {
		return err
	}
	if opts.JSON {
		return json.NewEncoder(stdout).Encode(headless.NewReport(res, model))
	}
	_, err = fmt.Fprintln(stdout, res.Output)
	return err
}
