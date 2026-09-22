package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/javanhut/ollama_code/internal/instructions"
	"github.com/javanhut/ollama_code/tools"
)

// instructionsEnabled reports whether AGENTS.md-style files are loaded.
func (m *Model) instructionsEnabled() bool {
	return m.cfg.ProjectInstructions == nil || *m.cfg.ProjectInstructions
}

// loadInstructions (re)reads the global, configured and project instruction
// files. The rendered block is cached: it rides the static system prompt, so it
// must be byte-stable between reloads or every request misses the KV cache.
func (m *Model) loadInstructions() {
	m.instructions = instructions.Set{}
	m.instructionsBlock = ""
	m.instructionTracker = nil
	if !m.instructionsEnabled() {
		return
	}
	cwd, _ := os.Getwd()
	m.instructions = instructions.Load(instructions.Options{Cwd: cwd, Extra: m.cfg.Instructions})
	m.instructionsBlock = m.instructions.Render()
	m.instructionTracker = instructions.NewTracker(cwd, m.instructions)
}

// pathArgTools are the tools whose "path" argument names a location the model
// is now working in, and so may pull in that directory's instruction file.
var pathArgTools = map[string]bool{
	"read_file": true, "list_directory": true, "edit_file": true, "multi_edit": true,
	"write_file": true, "append_file": true, "file_info": true, "get_project_tree": true,
}

// lazyInstructions returns instruction files from subdirectories the batch
// touched that the prompt does not carry yet, framed as an advisory. Only
// successful calls count: a failed read of a mistyped path is not work there.
func (m *Model) lazyInstructions(calls []tools.ToolCall) string {
	if m.instructionTracker == nil {
		return ""
	}
	var b strings.Builder
	for _, call := range calls {
		if !pathArgTools[call.Function.Name] {
			continue
		}
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(call.Function.Arguments, &args) != nil || args.Path == "" {
			continue
		}
		b.WriteString(m.instructionTracker.ForPath(args.Path))
	}
	return b.String()
}

// instructionsReport is the /instructions command output.
func (m *Model) instructionsReport() string {
	if !m.instructionsEnabled() {
		return "Project instructions are disabled (project_instructions: false in config)."
	}
	var b strings.Builder
	if len(m.instructions.Files) == 0 {
		b.WriteString("No instruction files loaded.\n\nLooked for AGENTS.md, OLLAMA.md, CLAUDE.md from " + m.instructions.Root +
			" down to the working directory, plus the global AGENTS.md in the config directory. Run /init to generate one.")
		return b.String()
	}
	b.WriteString("Instruction files in the system prompt (broad → specific):\n")
	for _, f := range m.instructions.Files {
		fmt.Fprintf(&b, "  • %s (%d bytes)\n", f.Path, len(f.Content))
	}
	for _, d := range m.instructions.Dropped {
		fmt.Fprintf(&b, "  ✗ %s — over the %d-byte budget\n", d, instructions.DefaultMaxBytes)
	}
	b.WriteString("\nAGENTS.md files in subdirectories are added when the model first works there. /instructions reload re-reads them.")
	return b.String()
}

// refreshInstructions re-reads the instruction files before a turn so an
// AGENTS.md written by /init, or edited in another window, takes effect on the
// next message without a restart. It is a handful of stat+read calls on small
// files; when nothing changed the block is byte-identical and the cached
// prefix survives. The lazy tracker is kept across an unchanged reload so a
// subdirectory's rules are not re-sent.
func (m *Model) refreshInstructions() {
	if !m.instructionsEnabled() {
		return
	}
	before := m.instructionsBlock
	tracker := m.instructionTracker
	m.loadInstructions()
	switch {
	case m.instructionsBlock == before:
		if tracker != nil {
			m.instructionTracker = tracker
		}
	case before == "":
		m.toast = fmt.Sprintf("loaded %d instruction file(s)", len(m.instructions.Files))
	default:
		m.toast = "instruction files changed — reloaded"
	}
}
