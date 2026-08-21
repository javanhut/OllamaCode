# Commands and keys

Type `/` in the input to get an autocomplete menu of every command.

## Debug logging

Start either the TUI or a headless run with `--debug` to create a fresh
`ocode.log` in the directory where ocode was launched:

```sh
ocode --debug
ocode --debug -p "diagnose why this tool call fails"
```

The file is newline-delimited JSON designed to be attached to an LLM for
debugging. It records the redacted model/tool lifecycle: message arrays sent to
the model, visible tools and decoding options, model content/reasoning/tool
calls, permission decisions, tool arguments/results and repair attempts,
verification, retries/errors, and turn/session completion. Each launch replaces
the previous `ocode.log`, so one file represents one run.

Common credential shapes and secret-bearing JSON/environment fields are
redacted, and the file is created with owner-only permissions. It can still
contain prompts, source code, file contents, command output, and uncommon secret
formats; inspect it before sharing. Add `ocode.log` to the target project's
`.gitignore` if you do not want Git to report it as untracked.

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
| `--debug` | Replace `./ocode.log` with a redacted model/tool execution trace |

Errors go to stderr with a non-zero exit code. The same confinement as the TUI
applies: file tools are jailed to the workspace and `run_shell` goes through
the OS sandbox. There is no approval prompt headless — running `ocode -p` is
itself the trust decision.

## Resuming sessions

Every completed turn is auto-saved (history, mode, model, workspace, todos,
session notes), so a session survives the process that ran it.

| Flag | Effect |
|---|---|
| `--resume` | Start the TUI restored from the last auto-saved turn |
| `--resume <name>` | Restore a named session written by `/save <name>` |

- Granularity is the **completed turn**: a turn that was still streaming when
  the process died is not saved; recovery picks up at the end of the last
  finished turn.
- If the previous run ended uncleanly (crash, kill, power loss), a plain
  `ocode` start says so and points at `--resume`; `ocode --resume` then
  restores the last completed turn and announces the recovery in a toast.
- `--resume` with nothing saved starts fresh and says "nothing to resume" —
  it never fails.
- `/rewind` and `/fork` move the conversation, not the files. `/rewind 2` drops
  your last two turns and everything that answered them, then continues from
  there; the discarded tail is saved as a `rewind_<timestamp>` session first, so
  it is never a one-way door. `/fork 2 try-b` writes that same point to a named
  session and leaves the live conversation alone — `/load try-b` picks the
  branch up later. Neither reverts a file edit; that is `/undo`, which rewinds
  one turn at a time to the workspace snapshot taken before it.
- `/undo` survives restarts: the stack is a per-workspace list of git tree ids
  (capped at 25 turns, newest first) and the trees live in the shadow repo, so
  after `--resume` you can still rewind file changes made before the process
  exited. See [Undo](safety.md#undo).
- `--resume` cannot be combined with `-p` — headless runs always start fresh.

State lives under the user config dir (`~/.config/ollama_code/` on Linux,
`~/Library/Application Support/ollama_code/` on macOS): `autosave.json` for
the last turn, `sessions/<name>.json` for `/save` sessions, `running.lock` as
the clean-exit marker, `checkpoints/<workspace-hash>.json` for the undo stack,
and `checkpoints/<workspace-hash>.git` for the shadow repo holding the
snapshots it points at. All writes are atomic (temp file + rename).

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

### Research

| Command | Description |
|---|---|
| `/research <question>` | Guided research turn: decomposes the question, searches the web, dedupes sources, reads the primary ones, and synthesizes an answer with a numbered source list |
| `/research` | On an existing research thread, continue/deepen it instead of restarting |

The recipe and its safety rules are described in [research.md](research.md).

### Session

| Command | Description |
|---|---|
| `/clear` | Reset the conversation |
| `/save [name]` | Save the session |
| `/load <name>` | Restore one |
| `/sessions` | List saved sessions |
| `/archive` | Retrieve compacted history from the KV archive |
| `/rewind [n]` | Drop the last n of your turns and continue from there (default 1) |
| `/fork [n] [name]` | Save the conversation as it stood n turns back as a new session |
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
| `/show_thinking`, `/thinking` | Show reasoning in an isolated live block and retain it with the completed answer |
| `/face` | Toggle the mascot overlay |
| `/welcome` | Toggle the startup panel |
| `/verify` | Toggle the auto compile-check after edits |
| `/companion` | Toggle the voice companion (speech in, speech out) |
| `/quit`, `/exit` | Exit |
