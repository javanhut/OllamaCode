# ollama_code

A terminal coding agent for Ollama. Chat with local models and let them inspect,
plan, and modify your codebase — with a safety boundary that assumes the model
will sometimes get it wrong.

![Go](https://img.shields.io/badge/Go-1.26-blue)
![bubbletea](https://img.shields.io/badge/framework-bubbletea-v2-purple)

[![Go CI](https://github.com/javanhut/OllamaCode/actions/workflows/ci.yml/badge.svg)](https://github.com/javanhut/OllamaCode/actions/workflows/ci.yml)

```sh
make build          # ./ocode
./ocode
./ocode --debug      # fresh redacted model/tool trace in ./ocode.log
```

You start in **explore** mode (read-only). `shift+tab` cycles to **plan**, then
**write**. Every change to a file shows you a diff and waits for `y`.

```
┌─ explore ──────┐   ┌─ plan ─────────┐   ┌─ write ────────┐
│ read, search,  │ → │ read + session │ → │ full toolset,  │
│ read-only shell│   │ notes only     │   │ each write     │
│                │   │                │   │ needs approval │
└────────────────┘   └────────────────┘   └────────────────┘
```

## Documentation

|                                            |                                                                      |
| ------------------------------------------ | -------------------------------------------------------------------- |
| [Getting started](docs/getting-started.md) | Install, first run, picking a model                                  |
| [Modes](docs/modes.md)                     | explore → plan → write, and why the mode is the safety boundary      |
| [Model routing](docs/routing.md)           | Big model plans, local model executes                                |
| [Commands](docs/commands.md)               | Every slash command and key binding                                  |
| [Tools](docs/tools.md)                     | What the model can do, and what each mode allows                     |
| [Configuration](docs/configuration.md)     | `config.json`, environment variables, per-model profiles             |
| [MCP servers](docs/mcp.md)                 | External tools over stdio or Streamable HTTP                         |
| [Safety](docs/safety.md)                   | Approval prompts, undo, loop guards, verification                    |
| [Architecture](docs/architecture.md)       | How a turn runs, for people changing the code                        |
| [Voice companion](docs/companion.md)       | Optional speech-in, speech-out popup                                 |
| [Training](docs/training.md)               | Export traces as a dataset and LoRA-tune a model on them             |
| [Evals](docs/eval.md)                      | Running cmd/eval, multi-sample pass rates, and the CI self-test gate |
| [Research](docs/research.md)               | Guided web research with deduped sources and citations               |

## What's in it

**Tuned for open-weight models.** Malformed tool JSON is salvaged; hallucinated
tool names get "did you mean…"; models whose template emits tool calls as text
still work; and on repeated bad arguments the model is re-asked under a JSON
schema to force a valid object.

**A verification gate.** A turn that edits files doesn't end on broken code — a
compile check runs (`go build`, `cargo check`, `tsc --noEmit`, or your own
command) and failures are fed back until it's green or a retry cap is hit. A
weak model can't declare success on code that never compiled.

**Loop safety.** A per-turn step budget plus repeated-call, oscillation,
re-read, and preamble-echo detection stop a confused model from spinning.

**Reliable edits.** `edit_file` matches in tiers (exact → whitespace-tolerant →
fuzzy), refuses edits that would break a file's syntax, and returns a diff.
`multi_edit` applies several replacements to one file atomically, and after
every edit the language server's errors for that file come straight back in the
tool result.
`parallel_edit` splits a large change into independent subtasks, plans each in
parallel, then applies them serially with conflict detection.

**Model routing.** Bind a model per mode — a big one to plan, a small local one
to write — and the endpoint, context window and toolset swap with it. Providers
can be any OpenAI-compatible server, a second Ollama daemon, or the local
Cursor agent CLI. See [routing](docs/routing.md).

**Auto-RAG.** Relevant code is embedded and retrieved each turn without a tool
call; the index refreshes incrementally as files change.

**Checkpoints and undo.** File changes are snapshotted per turn; `/undo` reverts
the last one.

**Dream mode.** After three minutes idle it drifts into background reflection —
candidate fixes, consolidated notes, promoted memory — and tells you what it
thought about when you come back.

**Project instructions and custom commands.** `AGENTS.md` / `CLAUDE.md`
files are loaded from the repo root down to the working directory (and lazily
from subdirectories the model works in); `/init` writes one. Markdown files in
`.ollama_code/commands/` become slash commands with `$ARGUMENTS` and
`` !`cmd` `` expansion — opencode and Claude Code command files work as-is.

**Precise approvals.** Approve once, for the turn, for the session (`s`), or
permanently (`A`). Shell rules are scoped to the subcommand (`git status *`, not
`git *`) and matched per piece of a compound command, so an approval can't be
smuggled into `git status && curl … | sh`.

**Scriptable.** `ocode -p` takes piped stdin (`git diff | ocode -p "review
this"`) and `--output-format stream-json` emits newline-delimited events for
tools and CI.

Plus per-model profiles discovered from `/api/show`, sub-agents, session notes,
save/load, token-budgeted prompt assembly, transcript search, and mouse
selection.

## Requirements

- Go 1.26+
- [Ollama](https://ollama.com) running locally, or an Ollama Cloud API key

## Development

```sh
make build              # ./ocode
make build-companion    # ./ollama-companion (optional, pulls in Gio)
make run
make test               # go test ./...
make check              # lint + test
make clean
```

See [Architecture](docs/architecture.md) for the package layout, the turn
lifecycle, and how to add a tool, a slash command, or a provider kind.

## License

MIT — see [LICENSE](LICENSE).
