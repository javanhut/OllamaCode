# Editor integration (Agent Client Protocol)

`ocode acp` runs ocode as an [Agent Client Protocol](https://agentclientprotocol.com)
agent: the editor launches it as a subprocess and talks JSON-RPC 2.0 over
stdin/stdout (newline-delimited). The same protocol is spoken by Zed, the
JetBrains IDEs, and Neovim and Emacs plugins.

Implemented against **ACP protocol version 1**. The JSON-RPC layer is
hand-written (no SDK dependency); stdout carries protocol messages only.

```sh
ocode acp [--model provider:model] [--max-steps N]
```

Model and host come from `config.json` exactly as for `ocode -p`; `--model`
overrides the default. If no model is configured the first prompt fails with a
message saying so.

## Zed

In `settings.json`:

```json
{
  "agent_servers": {
    "ocode": {
      "command": "ocode",
      "args": ["acp"]
    }
  }
}
```

Then open the agent panel and start a new "ocode" thread. Pass
`"args": ["acp", "--model", "ollama:qwen3-coder"]` to pin a model.

## Other editors

Any ACP client works: configure it to launch `ocode` with the argument `acp`
(JetBrains: the AI Assistant "custom agent" ACP config; Neovim: e.g.
CodeCompanion or avante ACP adapters; Emacs: `agent-shell`). The working
directory the editor sends in `session/new` becomes the workspace root.

## What is supported

| ACP | Behaviour |
|---|---|
| `initialize` | Version 1; `loadSession: false`; prompt capabilities `text`, `resource_link`, embedded text resources; no image/audio; no auth methods |
| `session/new` | `cwd` must be an absolute existing directory; becomes the workspace root (file jail, shell sandbox, memory, instruction files). `mcpServers` is ignored |
| `session/prompt` | Runs ocode's agent loop. Conversation history is kept per session for follow-up prompts |
| `session/update` | `agent_message_chunk`, `tool_call`, `tool_call_update` (pending, in_progress, completed/failed) with kind, title, locations, raw input, result text, and a diff for `write_file`/`edit_file` |
| `session/request_permission` | See below |
| `session/cancel` | Cancels the turn's context; the prompt returns `stopReason: "cancelled"`, including while a permission request is pending |

Stop reasons: `end_turn`, `max_turn_requests` (step limit or loop guard ended
the turn), `cancelled`. Prompt failures (e.g. the model host is down) are
returned as a JSON-RPC internal error.

Malformed input never crashes the server: bad JSON gets `-32700`, a frame
without a method `-32600`, unknown methods `-32601`, bad params `-32602`.

## Approvals

ACP sessions run with the interactive approval posture (auto mode off): reads
and other non-destructive tools run freely, while every destructive call
(writes, edits, deletes, shell commands, git mutations) is sent to the editor
as `session/request_permission` and blocks until you answer. Calls that touch
agent/editor config or git hooks, or that follow untrusted web/MCP content,
always ask. `deny` rules in `config.json` still apply and are never sent to the
editor.

| Option | Effect |
|---|---|
| `allow_once` | Run this call |
| `allow_always` | Run it, and allow the same tool (for `run_shell`, the same command prefix) for the rest of the session. Offered only when the call is not protected or untrusted |
| `reject_once` | Refuse; the model is told the user declined |
| `reject_always` | Refuse, and refuse the same tool/prefix for the rest of the session |

"Always" choices live in memory for the session only; nothing is written to
`config.json`. A cancelled outcome counts as a refusal.

## Not supported

- `session/load` / session resume (`loadSession: false`); history lives only
  as long as the `ocode acp` process.
- Client-side `fs/read_text_file` / `fs/write_text_file`: ocode reads and
  writes the disk directly, so unsaved editor buffers are not seen.
- `terminal/*`: shell commands run in ocode's own sandboxed shell, and their
  output is reported in the tool result, not a client terminal.
- Image and audio prompt content; `mcpServers` passed by the client;
  session modes and config options; `authenticate`.
- Token-level streaming: the model is called non-streaming, so each assistant
  message arrives as one `agent_message_chunk`.
- Concurrency: workspace root and cwd are process-global, so turns from
  different sessions run one at a time.
