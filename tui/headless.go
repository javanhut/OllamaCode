package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/headless"
	"github.com/javanhut/ollama_code/internal/instructions"
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
	Prompt   string
	Model    string // overrides the configured default model; accepts provider:model
	MaxSteps int    // tool-call rounds; 0 = configured max_steps, else the default
	JSON     bool   // emit a single JSON object instead of plain text (alias for OutputFormat "json")
	// OutputFormat is text, json, or stream-json (newline-delimited events as
	// the run progresses). Empty means text, or json when JSON is set.
	OutputFormat string
	DebugPath    string // fresh redacted JSONL trace; empty disables debug logging
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
	if cfg.Format != nil {
		tools.SetFormatEnabled(*cfg.Format)
	}
	tools.ConfigureLSP(cfg.LSP == nil || *cfg.LSP, workspaceRoot(), cfg.LSPServers)
	defer tools.CloseLSP()

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
	format := opts.OutputFormat
	if format == "" {
		format = headless.FormatText
		if opts.JSON {
			format = headless.FormatJSON
		}
	}

	// Same instruction files the TUI puts in its system prompt, so a repo's
	// AGENTS.md governs scripted runs too; nested ones attach lazily as the
	// run touches their directories.
	hopts := headless.Options{
		Model:       model,
		MaxSteps:    maxSteps,
		Trace:       recorder,
		Permissions: cfg.Permissions,
		NumCtx:      headlessNumCtx(cfg, host, spec, model),
		Sampling:    headlessSampling(cfg, spec, model),
	}
	if cfg.ProjectInstructions == nil || *cfg.ProjectInstructions {
		cwd, _ := os.Getwd()
		set := instructions.Load(instructions.Options{Cwd: cwd, Extra: cfg.Instructions})
		hopts.System = headless.DefaultSystem + set.Render()
		tracker := instructions.NewTracker(cwd, set)
		hopts.AugmentResult = func(call tools.ToolCall, result string) string {
			return tools.AppendEvidence(result, tracker.ForPath(headlessPathArg(call)))
		}
	}
	var stream *headless.StreamWriter
	if format == headless.FormatStreamJSON {
		stream = headless.NewStreamWriter(stdout)
		hopts.Stream = stream
		cwd, _ := os.Getwd()
		names := make([]string, 0)
		for _, def := range registry.Definitions() {
			names = append(names, def.Function.Name)
		}
		stream.Start(model, cwd, names)
	}

	res, err := headless.Run(ctx, host, registry, opts.Prompt, hopts)
	if stream != nil {
		// The terminal line goes out even on failure, so a consumer reading
		// the stream always sees how it ended; the error still sets the exit code.
		if werr := stream.Result(headless.NewReport(res, model), err); err == nil {
			err = werr
		}
		return err
	}
	if err != nil {
		return err
	}
	if format == headless.FormatJSON {
		return json.NewEncoder(stdout).Encode(headless.NewReport(res, model))
	}
	_, err = fmt.Fprintln(stdout, res.Output)
	return err
}

// headlessPathArg extracts the file path a tool call targets, for lazily
// attaching nested instruction files. "" when the call names no path.
func headlessPathArg(call tools.ToolCall) string {
	var args struct {
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(call.Function.Arguments, &args) != nil {
		return ""
	}
	if args.Path != "" {
		return args.Path
	}
	return args.FilePath
}

// headlessSampling applies the TUI's sampling rules to the cached profile. With
// no profile cached, the family preset (if any) still applies by name.
func headlessSampling(cfg config, spec, model string) map[string]any {
	var p ModelProfile
	for _, key := range []string{spec, model} {
		if cached, ok := cfg.Profiles[key]; ok {
			p = cached
			break
		}
	}
	opts, _ := resolveSampling(p, model, true)
	return opts
}

// headlessNumCtx picks the context window for a one-shot run the way
// resolveProfile does for the TUI: a cached profile, else /api/show, capped at
// maxContextBudget. Without it Ollama applies its small default window and
// silently truncates the system prompt and early tool results.
func headlessNumCtx(cfg config, host api.OllamaHost, spec, model string) int {
	if host.IsCursor() || host.IsOpenAI() {
		return 0
	}
	n := 0
	for _, key := range []string{spec, model} {
		if p, ok := cfg.Profiles[key]; ok && p.NumCtx > 0 {
			n = p.NumCtx
			break
		}
	}
	if n == 0 {
		n = defaultContextLimit
		if show, err := host.ShowModel(model); err == nil {
			if c := show.ContextLength(); c > 0 {
				n = c
			}
		}
	}
	return min(n, maxContextBudget)
}
