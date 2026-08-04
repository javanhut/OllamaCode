# Modes

The mode is the safety boundary. It decides which tools exist at all — not
which ones the model is asked nicely not to use. A tool that isn't allowed in
the current mode is never sent to the model, and a call for it is rejected
before dispatch.

```
 explore  ──shift+tab──▶  plan  ──shift+tab──▶  write  ──shift+tab──▶ explore
   │                        │                     │
 read-only              read + notes         full toolset
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

Output redirection (`>`, `>>`) and command substitution (`$(...)`, backticks)
are blocked, because either one turns a read into a write.

## plan — read plus notes

Everything explore allows **except shell**, plus the session-notes tools.

This is where the change gets designed: scope, files to touch, risks, the exact
diff strategy. The notes written here are the durable artifact — they are
re-injected into the prompt every turn, while ordinary chat history gets
truncated away as the context fills.

**Leaving plan mode for write mode requires a plan in notes.** A `switch_mode`
call is refused until the notes have actually changed since plan mode was
entered:

> `error: no plan recorded. Call update_session_notes with the complete plan —
> scope, the exact files to touch and the change in each, and the risks — then
> call switch_mode("write", ...) again.`

Staleness counts: notes left over from an earlier task do not satisfy the gate,
because they describe the wrong work. See [Safety](safety.md#the-plan-gate).

## write — full toolset

Everything. Every destructive call surfaces a permission prompt showing a diff
or the command, and waits for `y` / `a` / `n`.

On entering write mode from plan mode, the session notes are injected into
history as a `Plan Summary`, so the executing model starts from the plan.

## auto — autonomous

Full toolset with prompts suppressed for paths **inside the working directory**.
Anything outside it still prompts. The step budget rises from 25 to 100 rounds.

Only reachable via `/auto` or `/mode auto`. A model cannot switch itself into
auto mode — the tool call is rejected explicitly.

## How mode changes happen

| Trigger | Path |
|---|---|
| `shift+tab` | cycles explore → plan → write → explore |
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
