# ollama_code

A terminal UI chat client for Ollama with built-in filesystem and shell tool calling. Chat with local LLMs and let them inspect, plan, and modify your codebase — all from the terminal.

![Go](https://img.shields.io/badge/Go-1.26-blue)
![bubbletea](https://img.shields.io/badge/framework-bubbletea-v2-purple)

## Features

- **Streaming chat** with local Ollama models
- **Built-in tool calling** — the model can read, write, edit, delete, move, and copy files; run shell commands; search with `grep` and `find`
- **Three safety modes:**
  - `explore` — read-only; the model can only inspect files and directories
  - `plan` — read + session notes; the model can outline a plan without touching files
  - `write` — full access; destructive operations require your approval before running
- **Permission prompts** — in write mode, each destructive tool call is presented for approve/deny before execution
- **Session notes** — a persistent scratchpad the model uses to track decisions and context across turns
- **Mouse support** — click and drag to select text, auto-scrolls at edges, copies on release
- **Voice companion (optional)** — a small Gio popup window that captures your microphone for speech-to-text into the input box and speaks Layla's replies aloud via TTS
- **Slash commands** — `/help`, `/settings`, `/model`, `/notes`, `/clear`, `/copy`, `/companion`, `/quit`

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
│   └── api.go               # Ollama HTTP client (chat, streaming, models)
├── mcp/
│   ├── mcp.go               # Tool registry and 30+ built-in filesystem/shell tools
│   └── external.go          # JSON-RPC stdio bridge for external MCP servers
├── tui/
│   └── tui.go               # Bubbletea TUI (modals, viewport, input, modes)
├── internal/
│   ├── companion/           # CLI-side client that manages the companion subprocess
│   ├── huffman/             # Context compaction codec
│   ├── memory/              # Long-term user memory store
│   └── storage/             # Session KV store
├── companion/               # GUI logic (Gio): orb, audio capture, STT, TTS, IPC
├── cmd/
│   └── companion/
│       └── main.go          # Entry point for the `ollama-companion` binary
├── main.go                  # Entry point for `ollama-code`
└── go.mod                   # Module definition and dependencies
```

## Built-in Tools

| Tool | Description |
|---|---|
| `read_file` | Read a file with optional line range |
| `write_file` | Create or overwrite a file |
| `edit_file` | Replace exact text snippet in a file |
| `append_file` | Append text to a file |
| `delete_file` | Delete a file or directory |
| `move_file` | Move or rename a file/directory |
| `copy_file` | Copy a file |
| `list_directory` | List directory entries |
| `find_files` | Walk directory tree matching a glob pattern |
| `grep` | Search for regex patterns in files |
| `file_info` | Get metadata for a path |
| `make_directory` | Create directories |
| `touch` | Create empty file or update mtime |
| `run_shell` | Run arbitrary shell commands |
| `get_working_directory` | Return the current working directory |

## License

[MIT](LICENSE) (or your preferred license)
