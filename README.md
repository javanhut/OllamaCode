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
- **Slash commands** — `/help`, `/settings`, `/model`, `/notes`, `/clear`, `/copy`, `/quit`

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
| `/quit` | Exit |

### Permission Prompts (Write Mode)

| Key | Action |
|---|---|
| `Y` / `Enter` | Allow this tool call |
| `A` | Allow all pending calls in this turn |
| `N` / `Esc` | Deny this tool call |

## Development

The project includes a `Makefile` for common development tasks.

- **Setup dependencies:** `make setup`
- **Build the binary:** `make build`
- **Run the app:** `make run`
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
│   └── api.go          # Ollama HTTP client (chat, streaming, models)
├── mcp/
│   └── mcp.go          # Tool registry and 15 built-in filesystem/shell tools
├── tui/
│   └── tui.go          # Bubbletea TUI (modals, viewport, input, modes)
├── main.go             # Entry point
└── go.mod              # Module definition and dependencies
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
