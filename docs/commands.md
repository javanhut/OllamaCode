# Commands and keys

Type `/` in the input to get an autocomplete menu of every command.

## Headless mode

`ocode -p "..."` runs a single prompt non-interactively and prints the final
answer to stdout — no TUI, for scripts, git hooks, and CI. Without `-p`,
`ocode` starts the TUI as usual.

| Flag | Effect |
|---|---|
| `-p`, `--prompt` | Run one prompt headless and print the final answer |
| `-json` | Emit a single JSON object instead of plain text (`output`, `model`, `steps`, `tool_calls`, `tool_errors`, `tools_used`, `hit_limit`, token counts) |
| `-model` | Model for the run (default: the configured model); accepts `provider:model` |
| `-max-steps` | Cap tool-call rounds (default: configured `max_steps`) |

Errors go to stderr with a non-zero exit code. The same confinement as the TUI
applies: file tools are jailed to the workspace and `run_shell` goes through
the OS sandbox. There is no approval prompt headless — running `ocode -p` is
itself the trust decision.

```sh
# In a script: summarize the working tree as JSON for jq.
ocode -p "summarize uncommitted changes in one line" -json | jq -r .output
```

```sh
# .git/hooks/prepare-commit-msg: draft a commit message from the staged diff.
if ! ocode -p "Write a one-line commit message for the staged changes." > "$1.msg" 2>/dev/null; then
  rm -f "$1.msg"   # model unavailable — commit without a draft
else
  cat "$1.msg" >> "$1"
fi
```

```yaml
# CI: fail the job when the model can't explain the lint fallout.
- name: Explain lint failures
  run: ocode -p "Explain the golangci-lint failures above and name the files to fix." -json
```

## @file mentions

Type `@path` anywhere in a message to attach a file's contents to that turn —
e.g. `explain @tui/keys.go`. While typing an `@token`, `Tab` completes workspace
file paths from a menu (`↑`/`↓` to move, `Enter` to accept, `Esc` to dismiss).

- Paths resolve relative to the working directory and are confined to the
  workspace (the same jail the file tools use); escapes and missing files are
  noted inline for the model instead of failing the send.
- Files are capped at 32 KiB each (128 KiB per message), binary files are
  skipped, and a token only counts as a mention when it looks path-like
  (contains `/` or `.`), so `@handles` are left alone.
- The transcript shows your message as typed; the file contents are attached
  to the turn sent to the model.

## Keys

### Chat

| Key | Action |
|---|---|
| `Enter` | Send |
| `Shift+Enter` / `Ctrl+J` | Newline |
| `Shift+Tab` | Cycle mode: explore → plan → write → explore |
| `Esc` / `Ctrl+S` | Interrupt the running turn |
| `Ctrl+C` | Interrupt mid-turn; quit when idle |
| `↑` / `↓` | Recall previous messages (when the input is empty or unmodified) |
| `Ctrl+F` | Search the transcript |
| `n` / `N` | Next / previous match (with the search prompt dismissed) |
| `Ctrl+G` | Jump to the live end of the transcript |
| `Ctrl+T` | Expand or collapse tool call details |
| `Shift+↑/↓`, `PgUp/PgDn`, `Ctrl+U/D` | Scroll |
| drag with the mouse | Select transcript lines; release copies |

### Approval prompt

| Key | Action |
|---|---|
| `y` / `Enter` | Allow this call |
| `a` | Allow every pending call in this turn |
| `n` / `Esc` | Deny |

### Connection modal (`/settings`, `/provider`)

| Key | Action |
|---|---|
| `Tab` / `Shift+Tab` | Next / previous field |
| `↑` / `↓` | Switch endpoint (default host, each provider, + new provider) |
| `Space` / `←` / `→` | Cycle the wire format on the Wire row; toggle the Trust row |
| `Enter` | Save and test the selected endpoint |
| `Ctrl+D` | Delete the selected provider |
| `Esc` | Cancel |

### Model picker (`/models`)

| Key | Action |
|---|---|
| `↑` / `↓` or `k` / `j` | Move |
| `Enter` | Select |
| `p` | Pull a new model, with live progress |
| `r` | Refresh the list |
| `Esc` | Close |

## Slash commands

### Models and routing

| Command | Description |
|---|---|
| `/models` | Interactive list — switch or pull |
| `/model` | Show the active model's settings |
| `/model use <name>` | Set the default model. Accepts `provider:model` |
| `/model ctx <tokens>` | Override this model's context window |
| `/model temp <0.0–2.0>` | Override sampling temperature |
| `/model calibrate` | Run deterministic tool-use probes and recommend a capability tier |
| `/model calibrate apply` | Explicitly apply the latest recommendation |
| `/route` | Show the mode→model table |
| `/route <mode> <spec>` | Bind a model to a mode |
| `/route <mode> off` | Unbind one mode |
| `/route off` | Disable routing entirely |
| `/provider` | List configured endpoints |
| `/provider new` | Add one (modal) |
| `/provider <name>` | Edit one (modal) |
| `/provider remove <name>` | Delete it and any routes bound to it |
| `/settings` | Edit the default host's URL and key |
| `/settings <provider>` | Jump straight to that provider |

### Modes

| Command | Description |
|---|---|
| `/mode <explore\|plan\|write\|auto>` | Switch directly |
| `/auto` | Shortcut for `/mode auto` |

### Session

| Command | Description |
|---|---|
| `/clear` | Reset the conversation |
| `/save [name]` | Save the session |
| `/load <name>` | Restore one |
| `/sessions` | List saved sessions |
| `/archive` | Retrieve compacted history from the KV archive |
| `/undo` | Revert the file changes from the last turn |
| `/diff` | View the last turn's diffs full-screen |
| `/copy` | Copy the last response to the clipboard |
| `/stats` | Timing and token totals |

### Notes and memory

| Command | Description |
|---|---|
| `/notes` | Toggle the session-notes sidebar |
| `/clearnotes` | Clear the notes scratchpad |
| `/notes restore` | Restore notes from the pre-dream backup |
| `/dreams` | What it thought about while idle |
| `/dream` | Toggle idle dream mode |

### Display and behavior

| Command | Description |
|---|---|
| `/help`, `/?` | Help screen |
| `/verbose` | Toggle detailed tool output |
| `/show_thinking`, `/thinking` | Replay the model's reasoning in the transcript |
| `/face` | Toggle the mascot overlay |
| `/welcome` | Toggle the startup panel |
| `/verify` | Toggle the auto compile-check after edits |
| `/companion` | Toggle the voice companion (speech in, speech out) |
| `/quit`, `/exit` | Exit |
