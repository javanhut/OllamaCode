# Modes

The mode is the safety boundary. It decides which tools exist at all — not
which ones the model is asked nicely not to use. A tool that isn't allowed in
the current mode is never sent to the model, and a call for it is rejected
before dispatch.

```
 explore ─▶ plan ─▶ write ─▶ verify ─▶ explore  (shift+tab)
    │         │       │        │
read-only  notes   all tools  review + checks
```

`auto` is separate: it is only reachable by explicit user action (`/auto`),
never by a model's tool call.

## explore — read-only

Read files, search, grep, walk the project tree, use the semantic index, fetch
and search the web, and record session notes.

`run_shell` is available but filtered through a read-only allowlist per command
segment: `ls`, `cat`, `head`, `tail`, `grep`/`rg`, `find`/`fd`, `tree`, `wc`,
`file`, `stat`, `du`/`df`, `ps`, `env`, `which`, `sort`/`uniq`/`cut`/`tr`,
`basename`/`dirname`/`realpath`, plus `git status/log/diff/show/branch/remote/blame`
and `go version/env/list/doc/vet`.

Output redirection (`>`, `>>`), command substitution (`$(...)`, backticks) and
process substitution (`<(...)`) are blocked, because each one turns a read into
a write or a second command. Arguments are checked too: `find -exec/-delete`,
`fd --exec`, `rg --pre`, `sort -o`, `tree -o`, `env <command>`, `command <cmd>`
(only `command -v` is allowed), `git -c`, `git diff --output`, `git grep -O`,
`go vet -vettool` and `go env -w` are rejected, and `git branch`/`tag`/`remote`/
`reflog` are limited to their listing forms.

### Verified citations

Answers in explore mode must back every claim about the code with an inline
`path:line` citation — `tui/mode.go:42`, or `api/api.go:120-135` for a range.
When an answer finalizes, the harness parses each citation, resolves it
against the workspace, and checks that the file exists and the line number is
in range (`maybeCitationGate` in `tui/citations.go`, same re-invoke mechanics
as the compile-verify gate).

An answer that names source files but cites none — or whose citations don't
resolve — is sent back to the model once, with the exact list of which
citations failed and why. The retried answer is then shown as-is, so the gate
can't loop (the correction is detected in the history tail, not via extra
state). Pure explanations that name no source files are never challenged —
the "makes code claims" heuristic only triggers on a mention of a plausible
source path.

Citations render as underlined `path:line` references in the transcript. The
printable text is unchanged, so they stay greppable (`ctrl+f`) and copyable
(drag-select). There is no click-to-open in the transcript; to jump to a
cited location, copy the reference and ask about the file (an `@path` mention
works too).

## plan — read plus notes

Everything explore allows **except shell**, plus the session-notes tools.

This is where the change gets designed: scope, files to touch, risks, the exact
diff strategy. The notes written here are the durable artifact — they are
re-injected into the prompt every turn, while ordinary chat history gets
truncated away as the context fills.

**Leaving plan mode for write mode requires a reviewed plan in notes.** The
model records the plan, presents it with one focused `ask_user` confirmation,
and stops until you answer. A `switch_mode` call is refused until the notes
have actually changed since plan mode was entered and you have reviewed the
current version:

> `error: no plan recorded. Call update_session_notes with the complete plan —
> scope, the exact files to touch and the change in each, and the risks.`

Staleness counts: notes left over from an earlier task do not satisfy the gate,
because they describe the wrong work. See [Safety](safety.md#the-plan-gate).

Model tool calls cannot jump directly from explore to write. The harness rejects
that transition and requires explore → plan → reviewed plan → write. A user can
still force a mode with `/mode` or `shift+tab`.

## write — full toolset

Everything. Every destructive call surfaces a permission prompt showing a diff
or the command, and waits for `y` / `a` / `n`.

On entering write mode from plan mode, the session notes are injected into
history as a `Plan Summary`, so the executing model starts from the plan.

## verify — adversarial review

Read-only review of the explore findings, the plan and every line write
changed. `run_shell` uses the explore allowlist plus `go test`, which prompts.

`run_checks` (prompts) detects the project and runs its formatter check and
full test suite, and fails any changed source file without unit tests:

| Manifest | Format | Tests |
|---|---|---|
| `go.mod` | `gofmt -l` | `go test ./...` |
| `Cargo.toml` | `cargo fmt --check` | `cargo test` |
| `package.json` (npm/pnpm/yarn/bun by lockfile) | `format:check` script, else prettier or biome | `<pm> test` |
| Python (pip/uv/poetry/pipenv by lockfile) | `ruff format --check`, or `black --check` | `pytest` |

A verify turn cannot end until `run_checks` has passed on the current code.
On any failure the verdict is FAIL: findings go to the notes and the model
switches back to plan.

## auto — autonomous

Full toolset with prompts suppressed for paths **inside the working directory**.
Anything outside it still prompts. The step budget rises from 25 to 100 rounds.

Only reachable via `/auto` or `/mode auto`. A model cannot switch itself into
auto mode — the tool call is rejected explicitly.

## How mode changes happen

| Trigger | Path |
|---|---|
| `shift+tab` | cycles explore → plan → write → verify → explore |
| `/mode <name>` | jumps directly, including `auto` |
| `/auto` | shortcut for `/mode auto` |
| model calls `switch_mode` | goes through the approval prompt like any destructive tool |

All four funnel through one function, `applyModeTransition` in `tui/mode.go`.
That single choke point is why [model routing](routing.md) can swap the model,
its endpoint, and its context window on every transition without four separate
implementations.

## Tool visibility

The model only ever sees the tools its current mode allows, so it cannot even
propose an action the mode forbids. Small models additionally get a lean subset
— roughly file operations, search, shell, git, and mode switching — since a
40-tool schema drowns their instruction-following.

Full list: [Tools](tools.md).
