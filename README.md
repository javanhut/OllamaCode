# ollama_code

A terminal UI chat client for Ollama with built-in filesystem and shell tool calling. Chat with local LLMs and let them inspect, plan, and modify your codebase — all from the terminal.

![Go](https://img.shields.io/badge/Go-1.26-blue)
![bubbletea](https://img.shields.io/badge/framework-bubbletea-v2-purple)

## Features

- **Streaming chat** with local Ollama models
- **Built-in tool calling** — the model can read, write, edit, delete, move, and copy files; run shell commands; search with `grep` and `find`
- **Tuned for open-weight models** — a robustness layer makes weak/local models (Qwen-Coder, DeepSeek, GLM, Llama) behave:
  - **Tool-call repair** — malformed JSON arguments are salvaged; schema validation returns named, actionable errors; hallucinated tool names get "did you mean…" suggestions
  - **Text tool-call fallback** — models whose template emits tool calls as text (instead of the native channel) still work; calls are parsed from `<tool_call>`, `<function=…>`, or fenced JSON, with guardrails so prose is never hijacked
  - **Constrained-decoding escalation** — on repeated bad arguments, the model is re-asked with a JSON schema (`format`) to force a valid object
- **Verification gate** — when a turn edits files, the harness auto-runs a compile check (`go build`, `cargo check`, `tsc --noEmit`, or a configured command) before letting the turn end. A failing build is fed back and the model is **forced to keep fixing** until it's green or a retry cap is hit — so a weak model can't declare success on code that never compiled. When no objective check exists, it's challenged to prove it verified its work.
- **Loop safety** — a per-turn step budget, plus repeated-call and oscillation detection, stop a confused model from looping forever
- **Reliable edits** — `edit_file` matches in tiers (exact → whitespace/indent-tolerant → fuzzy similarity), rejects edits that would break a file's syntax before writing, and returns a unified diff
- **Auto-RAG** — relevant code is embedded and retrieved automatically each turn (no tool call needed); the index refreshes incrementally as files change
- **Per-model profiles** — context length (`num_ctx`) and tool support are discovered from Ollama's `/api/show` instead of hardcoded; sampling options are configurable per model
- **Sub-agents** — delegate read-only investigations (`spawn_subagent`) that run a bounded child loop and report back, without cluttering the main context
- **Checkpoints & undo** — file changes are snapshotted per turn; `/undo` reverts the last turn's edits
- **Dream mode** — after 3 minutes idle it drifts into background reflection: it dreams up candidate fixes and ideas, consolidates its notes, and promotes memory. Any prompt wakes it, and it tells you what it thought about while you were away (`/dreams` for the log, `/dream` to toggle, `/notes restore` to undo a consolidation)
- **Three safety modes:**
  - `explore` — read-only; the model can only inspect files and directories
  - `plan` — read + session notes; the model can outline a plan without touching files
  - `write` — full access; destructive operations require your approval before running
- **Permission prompts** — in write mode, each destructive tool call is presented for approve/deny before execution
- **Context management** — token-budgeted prompt assembly (never exceeds `num_ctx`), KV-prefix-cache-friendly ordering, and proactive Huffman-compressed compaction
- **Session notes** — a persistent scratchpad the model uses to track decisions and context across turns
- **Mouse support** — click and drag to select text, auto-scrolls at edges, copies on release
- **Voice companion (optional)** — a small Gio popup window that captures your microphone for speech-to-text into the input box and speaks Layla's replies aloud via TTS
- **Slash commands** — `/help`, `/settings`, `/model`, `/notes`, `/clear`, `/copy`, `/undo`, `/companion`, `/quit`

## Requirements

- Go 1.26+
- Ollama running locally (default: `http://localhost:11434`)

## Installation

```bash
go install github.com/javanhut/ollama_code@latest
```

Or build from source:

```bash
go build -o ollama_code .
./ollama_code
```

## Usage

Run the binary and connect to your Ollama instance:

1. Enter the Ollama URL (defaults to `http://localhost:11434`) and press Enter
2. Select a model from the list of locally available models
3. Start chatting — the model can use tools based on your current mode

```bash
./ollama_code
```

### Environment Variables

| Variable | Description |
|---|---|
| `OLLAMA_HOST` | Default Ollama URL (override the default `localhost:11434`) |
| `OLLAMA_MODEL` | Default model to pre-select |
| `OLLAMA_COMPANION_BIN` | Absolute path to the `ollama-companion` binary (overrides auto-discovery) |
| `OLLAMA_COMPANION_WHISPER_BIN` | Path to `whisper-cli` (default: `~/.cache/whisper/whisper-cli` or `$PATH`) |
| `OLLAMA_COMPANION_WHISPER_MODEL` | Path to a `ggml-*.bin` model (default: first match in `~/.cache/whisper/`) |
| `OLLAMA_COMPANION_PIPER_BIN` | Path to `piper` (default: `/opt/piper-tts/piper`, `~/.cache/piper/piper`, or `$PATH`) |
| `OLLAMA_COMPANION_PIPER_MODEL` | Path to a piper `.onnx` voice (default: first match in `~/.cache/piper/`) |

### Configuration

Settings persist to `~/.config/ollama_code/config.json`. Most are written automatically (host, model, per-model profiles), but you can edit the file to tune behavior:

| Field | Description |
|---|---|
| `host` | Ollama URL (defaults to `http://localhost:11434` when empty) |
| `model` | Last-selected chat model |
| `max_steps` | Tool-call budget per user turn before the agent stops and summarizes (default `25`) |
| `embed_model` | Embedding model used for auto-RAG (default `nomic-embed-text`) |
| `auto_rag` | Set to `false` to disable automatic retrieval (default enabled) |
| `dream` | Set to `false` to disable idle dream mode (default enabled) |
| `verify` | Set to `false` to disable the auto compile-check after edits (default enabled) |
| `verify_cmd` | Override the auto-detected compile check, e.g. `"go build ./... && go test ./..."` |
| `profiles` | Per-model `{num_ctx, supports_tools, temperature, top_p, num_predict}`, auto-discovered from `/api/show` and cached; edit to override sampling |

> **Auto-RAG** needs an embedding model pulled in Ollama (e.g. `ollama pull nomic-embed-text`). If it's missing, retrieval silently disables itself — the manual `code_index` / `semantic_search` tools remain available as a fallback.

### Keyboard Shortcuts

| Key | Action |
|---|---|
| `Enter` | Send message |
| `Shift+Enter` | Newline in input |
| `Tab` | Cycle modes (explore → plan → write) |
| `Shift+↑/↓` | Scroll viewport |
| `PgUp/PgDn` | Page up/down |
| `Ctrl+U/D` | Half page up/down |
| `Ctrl+C` | Quit |

### Slash Commands

| Command | Description |
|---|---|
| `/help` | Show help screen |
| `/settings` | Change Ollama URL |
| `/model` | Pick a model |
| `/notes` | View session notes |
| `/clear` | Reset the conversation |
| `/copy` | Copy last assistant response to clipboard |
| `/undo` | Revert the file changes made during the last turn |
| `/clearnotes` | Clear the session notes scratchpad (also `/notes clear`) |
| `/dreams` / `/dream` | Show the idle dream log / toggle dream mode |
| `/verify` | Toggle the auto compile-check after edits |
| `/save` / `/load` / `/sessions` | Save, load, and list conversation sessions |
| `/archive` | Retrieve a Huffman-compressed history archive |
| `/companion` | Toggle the voice companion popup (STT in → input, replies → TTS) |
| `/quit` | Exit |

### Permission Prompts (Write Mode)

| Key | Action |
|---|---|
| `Y` / `Enter` | Allow this tool call |
| `A` | Allow all pending calls in this turn |
| `N` / `Esc` | Deny this tool call |

## Voice Companion (optional)

A separate Gio-based GUI binary (`ollama-companion`) gives Layla a face and a voice. It captures your microphone, transcribes utterances via whisper.cpp, drops the text into the TUI's input box, and auto-sends — and it speaks each assistant reply back through piper. A small neon-blue orb visualizes mic level; its perimeter ripples with audio.

### System requirements

The companion shells out to local tools and reads its own audio. None are bundled.

| Component | Purpose | Install (Arch example) |
|---|---|---|
| **PipeWire or PulseAudio** | mic capture (`pw-cat` preferred, `parec` fallback) and playback (`paplay`) | usually preinstalled on modern Linux desktops |
| **whisper.cpp** | speech-to-text inference | `git clone https://github.com/ggerganov/whisper.cpp && cd whisper.cpp && cmake -B build && cmake --build build -j` |
| A whisper `ggml-*.bin` model | STT model weights | `bash models/download-ggml-model.sh base.en` (run inside the whisper.cpp checkout) |
| **piper** | text-to-speech inference | `yay -S piper-tts-bin` (AUR), or `pipx install piper-tts`, or download from [piper releases](https://github.com/rhasspy/piper/releases) |
| A piper voice (`.onnx` + `.onnx.json`) | TTS voice weights | download a pair from [rhasspy/piper-voices](https://huggingface.co/rhasspy/piper-voices) |
| **Gio system deps** | only at build time of `ollama-companion` | `vulkan-headers`, plus the usual X11/Wayland dev headers |

### Setup (auto-discovery layout)

If you place files in these locations, no env vars are needed:

```
~/.cache/whisper/
├── whisper-cli           # executable (or symlink to your whisper.cpp build)
└── ggml-base.en.bin      # any ggml-*.bin model; the first match wins

~/.cache/piper/
├── en_US-lessac-medium.onnx       # any *.onnx voice; the first match wins
└── en_US-lessac-medium.onnx.json  # piper voice config (sets sample rate)
```

The piper binary is auto-discovered at `/opt/piper-tts/piper`, `~/.cache/piper/piper`, `~/.local/bin/piper`, or anywhere on `$PATH` (as `piper-tts` or `piper`).

### Build and run

```bash
make build-companion       # produces ./ollama-companion
make build                 # produces ./ollama-code
./ollama-code
# Inside the TUI, type:
/companion                 # toggle on/off; speak — your words land in the input
```

Gio adds a sizable transitive dependency tree. The main `ollama-code` binary deliberately does **not** import `companion/`, so `make build` stays Gio-free; only `make build-companion` pulls in Gio.

### Window features

- **Drag** anywhere on the window to move it (compositor-mediated; works on X11 and Wayland).
- **Mute button** — small dot in the top-right corner; click to toggle. When muted, the orb dims, the wave goes flat, and STT is disabled. TTS-driven visualization still runs (Layla's voice still wobbles the orb).
- **Listening indicator** — the orb shifts to bright cyan with a faint outer halo while VAD is buffering an utterance, then snaps back when the utterance closes (~0.7 s of silence) and the transcript fires.
- **Diagnostic log** at `/tmp/ollama-companion.log` — `tail -f` it while using `/companion` to see capture stats, VAD events, and mute toggles.

### Wayland caveats

- **Always-on-top / corner placement**: Wayland clients cannot self-position or self-pin. Use a compositor rule. Example for Hyprland:
  ```
  windowrulev2 = float, class:^(ollama-companion)$
  windowrulev2 = pin,   class:^(ollama-companion)$
  windowrulev2 = move 100%-240 100%-240, class:^(ollama-companion)$
  ```
- **Frameless rendering**: honored by KDE/Sway/Hyprland; GNOME/Mutter may still draw server-side decorations.

## Development

The project includes a `Makefile` for common development tasks.

- **Setup dependencies:** `make setup`
- **Build the main binary:** `make build`
- **Build the companion (optional):** `make build-companion`
- **Run the app:** `make run`
- **Run the companion standalone:** `make run-companion` or `make dev-companion`
- **Clean build artifacts:** `make clean`

### Testing

The project has unit tests for the MCP tools and the API layer. To run all tests:

```bash
go test ./...
```

To run tests for specific packages:

```bash
go test ./mcp
go test ./api
```

## Project Structure

```
├── api/
│   └── api.go               # Ollama HTTP client (chat, streaming, embed, /api/show, ChatOnce)
├── mcp/
│   ├── mcp.go               # Tool registry and 45+ built-in tools
│   ├── validate.go          # Arg validation, nearest-tool (levenshtein), JSON schema
│   ├── parse.go             # Parse tool calls emitted as text (model-agnostic fallback)
│   ├── verify.go            # Pre-write syntax gate (go/parser, json)
│   ├── diff.go              # LCS unified diff for edit_file
│   ├── codeintel.go         # Comment-filter + output caps for code search
│   └── external.go          # JSON-RPC stdio bridge (dormant; not wired to config)
├── tui/
│   ├── tui.go               # Bubbletea TUI (modals, viewport, input, modes, agent loop)
│   ├── profile.go           # Per-model profiles + dynamic num_ctx
│   ├── tokens.go            # Token estimation
│   ├── assemble.go          # Token-budgeted prompt assembly
│   ├── loopguard.go         # Step cap, repeat/oscillation guards, JSON salvage, repair hints
│   ├── format_repair.go     # Constrained-decoding (format) escalation
│   ├── rag.go               # Auto-RAG: lazy build, per-turn retrieval, incremental reindex
│   ├── subagent.go          # spawn_subagent tool (read-only)
│   └── checkpoint.go        # Per-turn file snapshots for /undo
├── internal/
│   ├── agent/               # Headless agent loop (shared by sub-agents and eval)
│   ├── semantic/            # Embedding index: build, search, incremental reindex
│   ├── companion/           # CLI-side client that manages the companion subprocess
│   ├── huffman/             # Context compaction codec
│   ├── memory/              # Long-term user memory store
│   └── storage/             # Session KV store
├── companion/               # GUI logic (Gio): orb, audio capture, STT, TTS, IPC
├── cmd/
│   ├── companion/main.go    # Entry point for the `ollama-companion` binary
│   └── eval/main.go         # Headless benchmark harness
├── main.go                  # Entry point for `ollama-code`
└── go.mod                   # Module definition and dependencies
```

## Built-in Tools

| Tool | Description |
|---|---|
| `read_file` | Read a file with optional line range |
| `write_file` | Create or overwrite a file |
| `edit_file` | Replace a snippet — tiered matching (exact → whitespace-tolerant → fuzzy), syntax-gated, returns a diff |
| `append_file` | Append text to a file |
| `delete_file` | Delete a file or directory |
| `move_file` / `copy_file` | Move/rename or copy a file |
| `list_directory` / `find_files` | List entries / walk a tree matching a glob |
| `grep` | Search for regex patterns (output-capped) |
| `file_info` / `get_working_directory` | Path metadata / current directory |
| `make_directory` / `touch` | Create directories / empty files |
| `run_shell` | Run shell commands (read-only allowlist in explore mode) |
| `find_symbol` / `code_definition` / `code_references` / `code_hover` | Code intelligence (comment-filtered, capped) |
| `code_index` / `semantic_search` | Build and query the embedding index (also used automatically by auto-RAG) |
| `spawn_subagent` | Delegate a read-only investigation to a bounded child agent |
| `web_fetch` / `web_search` / `web_crawl` | Fetch and search the web |
| `git_*` | `status`, `diff`, `log`, `add`, `commit`, `branch`, `checkout`, `pull`, `push`, `stash`, `merge`, `reset`, `remote` |

All tool arguments are schema-validated before dispatch, and file-mutating tools are snapshotted so `/undo` can revert them.

## Evaluation

`cmd/eval` is a small, self-contained benchmark that runs scripted coding tasks through the headless agent loop against a model, so you can compare models and catch regressions:

```bash
go run ./cmd/eval -model qwen2.5-coder:7b
go run ./cmd/eval -model deepseek-coder-v2 -host http://localhost:11434
```

It reports pass/fail, step count, and timing per task. Requires Ollama with the named model pulled.

## License

[MIT](LICENSE) (or your preferred license)
