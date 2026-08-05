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
| `todo_write` | ✓ | ✓ | ✓ | ✓ |
| `switch_mode` | ✓ | ✓ | ✓ | ✓ |
| `ask_user` | ✓ | ✓ | ✓ | ✓ |
| `spawn_subagent` | ✓ | ✓ | ✓ | ✓ |

`remember` / `recall` / `forget` are invisible in the transcript — the model
gets the result, you just see the natural-language acknowledgement.

## Notable tools

### `spawn_subagent`

Delegates self-contained tasks to autonomous sub-agents with their own bounded
loop (20 rounds each). Passing multiple tasks runs up to 4 in parallel.

Sub-agents inherit the parent's mode, so they are read-only in explore and plan.
They cannot recurse, switch modes, or prompt the user.

Parallel sub-agents have **no cross-task conflict detection** and their edits
are not individually checkpointed for `/undo` — only parallelize work on
independent files.

### `todo_write`

Maintains a visible checklist. If a turn ends with items still open, the harness
nudges the model to keep going rather than letting it stop mid-task — bounded,
so a model that won't finish can't spin forever.

### `switch_mode`

Requests a mode transition. Treated as destructive, so it goes through the
approval prompt, and the preview names the model the switch would route to.

## Small-model toolset

Models under 15B parameters get a trimmed set — file operations, directory and
search tools, web search/fetch/crawl tools, `run_shell` / `shell_output`, the
common `git_*` tools, `switch_mode`, and `todo_write`. A 40-tool schema drowns their
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
