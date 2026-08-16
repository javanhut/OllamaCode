# Tools

The model acts only through tools. Which tools exist is decided by the
[mode](modes.md) — a tool the mode disallows is never sent to the model, and a
call for it is rejected before dispatch.

Legend: **E** explore · **P** plan · **W** write · **A** auto

## Files

| Tool | E | P | W | A |
|---|:-:|:-:|:-:|:-:|
| `read_file` | ✓ | ✓ | ✓ | ✓ |
| `list_directory` | ✓ | ✓ | ✓ | ✓ |
| `find_files` | ✓ | ✓ | ✓ | ✓ |
| `grep` | ✓ | ✓ | ✓ | ✓ |
| `file_info` | ✓ | ✓ | ✓ | ✓ |
| `get_project_tree` | ✓ | ✓ | ✓ | ✓ |
| `get_working_directory` | ✓ | ✓ | ✓ | ✓ |
| `hash_file` | ✓ | ✓ | ✓ | ✓ |
| `write_file` | | | ✓ | ✓ |
| `edit_file` | | | ✓ | ✓ |
| `append_file` | | | ✓ | ✓ |
| `delete_file` | | | ✓ | ✓ |
| `move_file` / `copy_file` | | | ✓ | ✓ |
| `make_directory` / `touch` | | | ✓ | ✓ |
| `parallel_edit` | | | ✓ | ✓ |

`write_file` and `edit_file` run the file's formatter before writing — `gofmt`,
`rustfmt`, `ruff format`, or `prettier`, chosen by extension. The formatter is
driven over stdin, so a formatter that rejects the input cannot leave a
truncated file behind, and one that is not installed is silently skipped. The
byte count, hash, and diff reported back describe the formatted result, so what
the model is told matches what is on disk. `append_file` is not formatted: it
carries fragments of a file that may not parse yet. Set `format: false` to
disable.

## Code intelligence

| Tool | E | P | W | A |
|---|:-:|:-:|:-:|:-:|
| `find_symbol` | ✓ | ✓ | ✓ | ✓ |
| `code_definition` | ✓ | ✓ | ✓ | ✓ |
| `code_references` | ✓ | ✓ | ✓ | ✓ |
| `code_hover` | ✓ | ✓ | ✓ | ✓ |
| `code_index` | ✓ | ✓ | ✓ | ✓ |
| `semantic_search` | ✓ | ✓ | ✓ | ✓ |

`code_index` and `semantic_search` use embeddings and always run against the
local Ollama daemon, never a routed provider.

### Language servers

`code_definition`, `code_references`, and `code_hover` ask a real language
server first when one is installed for the file's language: `gopls`,
`pyright-langserver`, `typescript-language-server`, and `rust-analyzer` are
known without configuration, and more can be declared in
[`lsp_servers`](configuration.md#lsp_servers). A compiler's index knows which
same-named identifiers actually are the symbol, which neither the tree-sitter
path nor the word-boundary grep below it can tell.

Servers start on the first code intelligence question, not at boot, and are
shut down when the session exits. Every failure — no server installed, a server
that will not start, one that does not answer in time — declines to the next
tier rather than erroring, so this is never a dependency. `lsp: false` in the
config turns the tier off entirely.

### Tree-sitter precision (optional build)

By default `find_symbol`, `code_definition`, and `code_references` are
grep/regex-based so the binary stays CGO-free. Building with
`make build-ts` (equivalently `go build -tags treesitter`) swaps in a
tree-sitter path for Go, Python, Rust, JavaScript, and TypeScript/TSX files:
definitions and references come from the parsed AST, so comments and string
literals no longer match, doc comments attach to definitions, and declarations
the regexes miss (e.g. `func Hello()` with no space before the parenthesis)
are found. Files in other languages, and files that fail to parse, silently
fall back to the regex behavior per file, and the result format, limits, and
caps are unchanged either way.

## Shell and processes

| Tool | E | P | W | A |
|---|:-:|:-:|:-:|:-:|
| `run_shell` | allowlist | | ✓ | ✓ |
| `shell_output` | | | ✓ | ✓ |
| `process_list` | ✓ | ✓ | ✓ | ✓ |
| `disk_usage` | ✓ | ✓ | ✓ | ✓ |
| `process_kill` | | | ✓ | ✓ |
| `env_get` / `env_list` / `env_set` | | | ✓ | ✓ |

In explore mode `run_shell` is filtered per command segment against a read-only
allowlist, with redirection and command substitution blocked. In plan mode it is
unavailable entirely. See [Modes](modes.md#explore--read-only).

## Version control

| Tool | E | P | W | A |
|---|:-:|:-:|:-:|:-:|
| `git_status` / `git_diff` / `git_log` | ✓ | ✓ | ✓ | ✓ |
| `git_branch` / `git_remote` | ✓ | ✓ | ✓ | ✓ |
| `git_add` / `git_commit` | | | ✓ | ✓ |
| `git_checkout` / `git_pull` / `git_push` | | | ✓ | ✓ |
| `git_stash` / `git_merge` / `git_reset` | | | ✓ | ✓ |

In a repository managed by [ivaldi](https://github.com/javanhut/ivaldi), a bare
`git` command sent through `run_shell` is intercepted and rejected — it would
bypass the translation layer and fail with "not a git repository". The `git_*`
tools translate transparently. `detectVCS` walks up the tree and returns
`ivaldi` first when both `.ivaldi/` and `.git/` are present.

## Web

| Tool | E | P | W | A |
|---|:-:|:-:|:-:|:-:|
| `web_fetch` | ✓ | ✓ | ✓ | ✓ |
| `web_search` / `web_search_api` | ✓ | ✓ | ✓ | ✓ |
| `web_crawl` | ✓ | ✓ | ✓ | ✓ |

## Session state

| Tool | E | P | W | A |
|---|:-:|:-:|:-:|:-:|
| `read_session_notes` | ✓ | ✓ | ✓ | ✓ |
| `update_session_notes` / `append_session_notes` | ✓ | ✓ | ✓ | ✓ |
| `remember` / `recall` / `forget` | ✓ | ✓ | ✓ | ✓ |
| `todo_write` / `todo_read` | ✓ | ✓ | ✓ | ✓ |
| `switch_mode` | ✓ | ✓ | ✓ | ✓ |
| `ask_user` | ✓ | ✓ | ✓ | ✓ |
| `spawn_subagent` | ✓ | ✓ | ✓ | ✓ |

`remember` / `recall` / `forget` are invisible in the transcript — the model
gets the result, you just see the natural-language acknowledgement.

## Notable tools

### `spawn_subagent`

Delegates self-contained tasks to autonomous sub-agents with their own bounded
loop (20 rounds each). Passing multiple tasks runs up to 4 in parallel.

By default (`async: true`) the call is **non-blocking**: it returns immediately
with a job id and the sub-agents run in the background. When the job finishes,
its full report is injected into the conversation as a completion notification
(`[SUB-AGENT JOB n COMPLETE] …`) — the parent is woken to react if it was idle,
so results arrive without polling. Up to 4 background jobs may run at once; esc
cancels them (mid-turn via the normal interrupt, or when idle). Pass
`async: false` for the old blocking behavior: the call returns only once every
sub-agent has reported, with the reports inline as the tool result.

Sub-agents inherit the parent's mode, so they are read-only in explore and plan.
They cannot recurse, switch modes, or prompt the user.

Parallel sub-agents have **no cross-task conflict detection** — only
parallelize work on independent files. Their file edits are checkpointed for
`/undo`, so a single `/undo` rewinds a delegation (individual sub-agent edits
are not separately undoable). Checkpointing caveat for background jobs: a
synchronous spawn banks every edit into the spawning turn, but a background
sub-agent that is still running when that turn ends banks its later edits into
whichever turn checkpoint is open when each write happens — so `/undo` for
those detached writes is attributed to the later turn, not the spawning one.

### `parallel_edit`

Splits a large change into independent subtasks planned by read-only workers in
parallel, then applies their staged edits serially through the safe write path.

The apply is **atomic**: if any staged change fails (a stale `old_string`, an
overlap between workers, …), every change the batch already applied is rolled
back — files restored to their pre-batch content, files the batch created
removed — and the tool returns a retryable error naming the failed change and
what was rolled back. Nothing is ever left half-applied; the model fixes the
cause and re-runs the whole call. The rollback restores content only — the turn
checkpoint is untouched, so `/undo` still rewinds the turn afterwards.

### `todo_write` / `todo_read`

Maintains a visible checklist (`todo_read` returns it as JSON in the same shape
`todo_write` accepts, so read-modify-write round-trips are trivial). If a turn
ends with items still open, the harness nudges the model to keep going rather
than letting it stop mid-task — bounded, so a model that won't finish can't
spin forever.

### `switch_mode`

Requests a mode transition. Treated as destructive, so it goes through the
approval prompt, and the preview names the model the switch would route to.

### `ask_user`

Creates a real conversation boundary. Ocode runs only the question from that
batch, cancels other unstarted calls, and does not invoke the model again until
the user replies. In plan mode, asking after recording notes establishes the
review checkpoint required before requesting write mode; changing the notes
requires another confirmation.

## Small-model toolset

Models under 15B parameters get a trimmed set — file operations, directory and
search tools, web search/fetch/crawl tools, `run_shell` / `shell_output`, the
common `git_*` tools, `switch_mode`, and `todo_write` / `todo_read`. A 40-tool schema drowns their
instruction-following and produces malformed calls.

They are also told to call exactly one tool per response; larger models are told
to batch independent calls, which then run in parallel.

## Reliability machinery

Every call goes through the same path:

- **JSON salvage** repairs almost-valid arguments before dispatch
- **Constrained-decoding repair** — on an argument error, the model is asked
  once for schema-valid arguments via a JSON schema, then the call is retried
- **Per-tool timeouts** — 30s for inspection, 90s for local mutation, 2min for
  network, 10min for sub-agents; `run_shell` uses its own requested timeout
  capped at 300s
- **Panic recovery** — a panicking handler becomes an error result, not a crash
- **Identical-failure short circuit** — the same call with the same arguments
  failing repeatedly is refused rather than retried
