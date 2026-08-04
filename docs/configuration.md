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
| `CURSOR_API_KEY` | Read by `cursor-agent` when a cursor provider has no key of its own |
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
| `model` | Default model — used by any mode without a route |
| `max_steps` | Tool-call rounds per turn before the agent stops and summarizes (default 25; auto mode uses 100) |
| `embed_model` | Embedding model for auto-RAG (default `nomic-embed-text`) |
| `auto_rag` | `false` disables automatic retrieval |
| `dream` | `false` disables idle reflection |
| `verify` | `false` disables the compile check after edits |
| `verify_cmd` | Override the auto-detected check, e.g. `"go build ./... && go test ./..."` |
| `face` | `false` hides the mascot overlay |
| `welcome` | `false` hides the startup panel |
| `verbose` | Detailed tool output |
| `show_thinking` | Replay reasoning in the transcript |
| `profiles` | Per-model settings, see below |
| `routes` | Mode → model spec, see [routing](routing.md) |
| `providers` | Extra endpoints, see below |

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
    "temperature": 0.2
  }
}
```

| Field | Meaning |
|---|---|
| `num_ctx` | Context window. Capped at 131072 regardless of what the model reports |
| `supports_tools` | Whether tools are sent at all |
| `supports_thinking` | Whether the reasoning stream is requested |
| `params_b` | Parameter count in billions. Under 15 triggers the small-model tier: compact prompt, lean toolset, temperature 0.2. `0` means unknown and is treated as large |
| `temperature`, `top_p`, `num_predict` | Sampling overrides; omit to use the model's defaults |

`/model ctx` and `/model temp` write here.

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
    "api_key_env": "CURSOR_API_KEY"
  },
  "workstation": {
    "base_url": "http://192.168.1.50:11434",
    "kind": "ollama"
  }
}
```

| Field | Meaning |
|---|---|
| `base_url` | HTTP endpoint for `openai`/`ollama`; the path to the binary for `cursor` (blank → PATH) |
| `api_key` | Stored key. Written by the modal's Key field |
| `api_key_env` | Environment variable holding the key. Wins over `api_key` |
| `kind` | `openai` (default), `ollama`, or `cursor` |

Provider names cannot contain `:` or spaces — the colon separates provider from
model in a route spec.

## Other state

| Path | Contents |
|---|---|
| `~/.ollama_code/archive.json` | KV archive of compacted conversation history |
| `~/.ollama_code/user_memory.json` | Long- and short-term memory from `remember` |
| session saves | Written by `/save`, listed by `/sessions` |

## Editing while running

OllamaCode writes the whole config on many actions (model switch, route change,
toggles), so edits made while it's running will be overwritten. Quit first.
