# Configuration

Everything lives in `~/.config/ollama_code/config.json`. Most of it is written
for you — host, model, per-model profiles, routes, providers — but the file is
plain JSON and safe to edit while OllamaCode isn't running.

## Environment variables

| Variable | Effect |
|---|---|
| `OLLAMA_HOST` | Default Ollama URL, overriding `localhost:11434` |
| `OLLAMA_MODEL` | Model to pre-select |
| `OLLAMA_API_KEY` | Bearer token for the default host. Takes precedence over `api_key` in the config, so the secret never has to touch disk |
| `CURSOR_API_KEY` | Read by the Cursor agent CLI when a cursor provider has no key of its own |
| `OLLAMA_COMPANION_BIN` | Path to the `ollama-companion` binary |
| `OLLAMA_COMPANION_WHISPER_BIN` | Path to `whisper-cli` |
| `OLLAMA_COMPANION_WHISPER_MODEL` | Path to a `ggml-*.bin` model |
| `OLLAMA_COMPANION_PIPER_BIN` | Path to `piper` |
| `OLLAMA_COMPANION_PIPER_MODEL` | Path to a piper `.onnx` voice |

Any provider can name its own environment variable in the **Env** field, which
outranks a stored key. Prefer that over typing the key.

## Top-level fields

| Field | Description |
|---|---|
| `host` | Default Ollama URL (empty → `http://localhost:11434`) |
| `api_key` | Bearer token for the default host; overridden by `OLLAMA_API_KEY` |
| `model` | Default model — used by any mode without a route. May carry a provider prefix (`cursor:auto`); a bare name means the default host |
| `max_steps` | Tool-call rounds per turn before the agent stops and summarizes (default 25; auto mode uses 100) |
| `embed_model` | Embedding model for auto-RAG (default `nomic-embed-text`) |
| `auto_rag` | `false` disables automatic retrieval |
| `dream` | `false` disables idle reflection |
| `verify` | `false` disables the compile check after edits |
| `verify_cmd` | Override the auto-detected check, e.g. `"go build ./... && go test ./..."` |
| `format` | `false` disables running the file's formatter on write; on by default when the formatter is installed |
| `lsp` | `false` disables language-server-backed code intelligence; on by default when a server is installed |
| `lsp_servers` | Extra or overriding language servers, see below |
| `permissions` | Allow/ask/deny rules for tool calls, see below |
| `trace` | Opt in to redacted JSONL execution tracing (off by default) |
| `trace_path` | Optional trace destination; defaults to the OS cache directory |
| `shell_sandbox` | `false` disables the OS-level sandbox around `run_shell` (sandbox-exec on macOS, bwrap on Linux); on by default |
| `jail_allowlist` | List of extra absolute directory roots the file tools may touch and the shell sandbox may write, beyond the workspace |
| `face` | `false` hides the mascot overlay |
| `welcome` | `false` hides the startup panel |
| `verbose` | Detailed tool output |
| `show_thinking` | Show reasoning live in an isolated block and retain it in the transcript |
| `profiles` | Per-model settings, see below |
| `routes` | Mode → model spec, see [routing](routing.md) |
| `providers` | Extra endpoints, see below |
| `mcp_servers` | External MCP stdio servers, see below |

## `profiles`

Keyed by model name. Discovered from `/api/show` and cached, so `num_ctx` and
tool support match the actual model rather than a hardcoded guess. Edit to
override.

```json
"profiles": {
  "qwen3-coder:30b": {
    "num_ctx": 32768,
    "supports_tools": true,
    "supports_thinking": false,
    "params_b": 30.5,
    "capability_tier": "capable",
    "parallel_tool_calls": true,
    "rag_tokens": 6000,
    "rag_top_k": 8,
    "temperature": 0.2
  }
}
```

| Field | Meaning |
|---|---|
| `num_ctx` | Context window. Capped at 131072 regardless of what the model reports |
| `supports_tools` | Whether tools are sent at all |
| `supports_thinking` | Whether the reasoning stream is requested |
| `params_b` | Parameter count in billions. Under 15 triggers the small-model tier: compact prompt, lean toolset, temperature 0 on tool-capable turns and 0.2 on tool-less prose turns. `0` means unknown and is treated as large |
| `capability_tier` | Optional `small`, `capable`, or `strong` override for size-based tiering |
| `max_visible_tools` | Optional cap used by task-aware tool selection |
| profile `max_steps` | Per-model tool-round budget, overriding the top-level default |
| `parallel_tool_calls` | Override whether the model is instructed to batch independent calls |
| `max_parallel_tools` | Maximum tool calls executed concurrently; defaults to 1 for small models and 4 otherwise |
| `delegation` | Override whether `spawn_subagent` is exposed to the model |
| `rag_tokens`, `rag_top_k` | Override retrieval context size and result count; small models default to 2,200 tokens and 4 results |
| `action_temperature`, `prose_temperature` | Separate sampling controls for tool-capable and tool-less turns |
| `review_pass` | Run an adversarial post-build review; defaults on for the explicit `strong` tier |
| `temperature`, `top_p`, `num_predict` | Sampling overrides; omit to use the model's defaults |

`/model ctx` and `/model temp` write here.

### Token estimation

Prompt budgeting, compaction, and the mid-turn ctx meter estimate tokens with a
chars-per-token ratio. The default is the ~4-chars-per-token heuristic; running
`/model calibrate` measures the model's real ratio by comparing known prompt
lengths against the host's `prompt_eval_count`, and the estimators use the
measured value from then on. The ratio is stored with the calibration result in
the OS cache directory (`/ollama_code/calibration/`) and mirrored to
`~/.config/ollama_code/token_ratios.json`, keyed by `provider|model`, so later
sessions pick it up on the first render. Models that were never calibrated — or
whose host reports no `prompt_eval_count` — stay on the default heuristic.

## `routes`

```json
"routes": {
  "plan":  "openrouter:anthropic/claude-sonnet-4",
  "write": "qwen3-coder:30b"
}
```

Keys are mode names. Values are `<model>` on the default host or
`<provider>:<model>`. A mode with no entry falls back to `model`. An empty or
missing `routes` object disables routing entirely.

## `providers`

```json
"providers": {
  "openrouter": {
    "base_url": "https://openrouter.ai/api/v1",
    "api_key_env": "OPENROUTER_API_KEY",
    "kind": "openai"
  },
  "cursor": {
    "kind": "cursor",
    "api_key_env": "CURSOR_API_KEY",
    "trust": true
  },
  "workstation": {
    "base_url": "http://192.168.1.50:11434",
    "kind": "ollama"
  }
}
```

| Field | Meaning |
|---|---|
| `base_url` | HTTP endpoint for `openai`/`ollama`; the path to the agent binary for `cursor` (blank → found on PATH, see [routing](routing.md#offloading-planning-to-the-cursor-agent)) |
| `api_key` | Stored key. Written by the modal's Key field |
| `api_key_env` | Environment variable holding the key. Wins over `api_key` |
| `kind` | `openai` (default), `ollama`, or `cursor` |
| `trust` | cursor only: pass `--trust`, suppressing Cursor's workspace-trust prompt. Off by default; without it headless runs abort |

Provider names cannot contain `:` or spaces — the colon separates provider from
model in a route spec.

## MCP servers

External MCP servers run as local stdio subprocesses or Streamable HTTP
endpoints — see [MCP servers](mcp.md) for HTTP and auth configuration. Their
tools are namespaced as `mcp_<server>_<tool>` to avoid collisions with
built-ins.

```json
"mcp_servers": {
  "docs": {
    "command": "npx",
    "args": ["-y", "@example/docs-mcp"],
    "trusted": true,
    "read_only": true,
    "small_model_safe": true,
    "env_allow": ["HOME"],
    "call_timeout_sec": 60,
    "max_response_kb": 1024
  }
}
```

| Field | Meaning |
|---|---|
| `command` | Executable to launch directly; no shell expansion is performed |
| `args` | Command arguments |
| `trusted` | Required explicit trust decision. An untrusted server is not launched |
| `work_dir` | Explicit child working directory; blank inherits the OllamaCode working directory |
| `env_allow` | Environment variable names passed to the child in addition to `PATH`; everything else is withheld |
| `call_timeout_sec` | Per-request deadline; defaults to 120 seconds |
| `max_response_kb` | Maximum JSON-RPC response size; defaults to 4096 KiB |
| `protocol_version` | Optional stateful MCP protocol version override; defaults to `2025-11-25` |
| `read_only` | Expose tools in Explore and Plan without destructive prompts; only set this when every server tool is actually read-only |
| `small_model_safe` | Permit the server's tools in the small-model candidate set |
| `disabled` | Keep the configuration without launching the server |

Unclassified MCP tools default to Write/Auto mode and require approval. Marking
a whole server read-only is a trust decision because MCP annotations are
advisory rather than a security boundary. Servers advertising tool-list change
notifications are refreshed atomically while the session is running.

## `permissions`

Rules narrow or widen the approval prompt for a tool call. Without any rules the
built-in behavior is unchanged: the mode decides what is available, and every
destructive call in write mode asks.

```json
"permissions": [
  { "tool": "run_shell", "cmd": "npm *", "effect": "allow" },
  { "tool": "write_file", "effect": "allow" },
  { "tool": "*", "path": "*.env", "effect": "deny" },
  { "tool": "git_push", "effect": "ask" }
]
```

| Field | Meaning |
|---|---|
| `tool` | Glob over the tool name: `write_file`, `git_*`, `*` |
| `path` | Glob matched against the call's target paths. `secrets/**` is a subtree; a pattern with no `/` matches the base name at any depth |
| `cmd` | Prefix match against `run_shell`'s command when it ends in `*`, otherwise an exact match |
| `effect` | `allow` skips the prompt, `ask` forces it, `deny` rejects the call |

`deny` outranks `ask`, which outranks `allow`, regardless of the order rules are
written in — a broad `allow` can never widen past a narrow `deny`. A rule naming
`path` or `cmd` only matches calls that actually carry that kind of resource.

`deny` is the only effect enforced outside the prompt: it also stops the call in
a headless run (`ocode -p`) and inside a spawned subagent, where there is nobody
to ask. A malformed glob matches nothing, so a typo fails closed.

Pressing `A` at the permission prompt writes an `allow` rule for the current
call and saves it here. The modal shows the exact rule before you press it.

## `lsp_servers`

Language servers back `code_definition`, `code_references`, `code_hover`, and
the diagnostics folded into a failed verification. `gopls`, `pyright-langserver`,
`typescript-language-server`, and `rust-analyzer` are known without
configuration; a server that is not installed is skipped silently and the
grep-based path answers instead.

```json
"lsp_servers": {
  "zls": {
    "command": "zls",
    "extensions": [".zig"],
    "root_markers": ["build.zig"],
    "language_id": "zig"
  }
}
```

| Field | Meaning |
|---|---|
| `command` | Binary to launch; skipped when not on PATH |
| `args` | Arguments, e.g. `["--stdio"]` |
| `extensions` | File extensions this server answers for, including the dot |
| `root_markers` | Files identifying the project root; the nearest match walking up from the file wins, otherwise the workspace root |
| `language_id` | Protocol language identifier; derived from the extension when omitted |

Using a built-in's name (`gopls`, `pyright`, `typescript`, `rust-analyzer`)
replaces it — useful when the binary lives somewhere unusual. Servers start on
the first code intelligence question, not at boot, and are shut down on exit.

## Other state

| Path | Contents |
|---|---|
| `~/.ollama_code/memory/<workspace>.json` | Long-term memory from `remember`, **scoped per workspace** |
| `~/.ollama_code/archive.json` | KV archive of compacted conversation history |
| session saves | Written by `/save`, listed by `/sessions` |
| OS cache directory `/ollama_code/trace.jsonl` | Optional redacted execution trace |
| OS cache directory `/ollama_code/calibration/` | Versioned model calibration results |

### Memory scoping

Long-term memory is injected into every prompt, so a single shared store meant
every project's facts turned up in every other project's context. Each workspace
gets its own file instead.

The workspace is the **enclosing repository** — the nearest parent holding
`.ivaldi` or `.git` — so launching from a subdirectory uses the same store as
the repo root. Outside a repository it's the working directory.

The filename is the directory name plus a hash of the full path, so two
checkouts sharing a basename don't share memory:

```
~/.ollama_code/memory/OllamaCode-9586baa4.json
```

Short-term memory stays in-process and never touches disk.

**Pre-scoping memory is not migrated.** The old shared store stays at
`~/.ollama_code/user_memory.json`, untouched — importing it would spread one
project's accumulated notes into whichever workspace opened first, which is the
noise this removes. A workspace with no memory of its own says where the old
file is, once, until it remembers something. To adopt the old entries in a
specific repo, copy the file to that workspace's path above.

## Editing while running

OllamaCode writes the whole config on many actions (model switch, route change,
toggles), so edits made while it's running will be overwritten. Quit first.
