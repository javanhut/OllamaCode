# Safety and guardrails

Nothing here relies on the model behaving. Each item is enforced by the harness,
because the whole point is running models that sometimes don't.

## Mode gating

The first line. A tool the current [mode](modes.md) disallows is never sent to
the model, so it cannot propose the action, and a call for it is rejected before
dispatch. Shell in explore mode is additionally filtered per command segment
against a read-only allowlist, with redirection and command substitution
blocked.

## Approval prompts

Every destructive call in explore, plan and write mode surfaces a prompt with a
preview — a diff for file edits, the command line for shell, the target for
git operations.

```
y / Enter   allow once
a           allow every pending call this turn
n / Esc     deny
```

Denial is recorded as a failure of that exact call, so an immediate identical
retry is short-circuited and the reject-retry loop shows up to the oscillation
detector.

In auto mode, prompts are suppressed only for paths **inside the working
directory**. Anything outside still asks.

## Undo

Files are snapshotted before any mutating tool runs, and the turn's changes are
banked as one checkpoint when it ends.

```
/undo       revert the last turn's file changes
/diff       view them first
```

Parallel sub-agent edits are not individually checkpointed — see
[Tools](tools.md#spawn_subagent).

## The plan gate

Leaving plan mode for write mode requires a plan in session notes. The model's
`switch_mode` call is refused until the notes have changed since plan mode was
entered — stale notes from an earlier task don't count.

The notes are the handoff: they are re-injected every turn while chat history
gets truncated away as the context fills, and when routing is configured the
executing model may be a different model entirely that never saw the planning
conversation.

The refusal is deliberately not counted as a failed call, because the intended
recovery is to write the notes and retry that same call. A model that ignores
the instruction and just retries is caught by the repeated-action guard instead.

Forcing the switch with `shift+tab` still works — that's your call — but the
toast says `write mode — no plan in notes, nothing was handed off`.

## Plan verification

When planning was [offloaded to another model](routing.md#plan-verification):

- A plan naming **no files at all** is a question or a refusal, not a plan, and
  never reaches write mode.
- A file the plan named **cannot be edited until it has been read** that turn.

Both are mechanical. The prompt also asks the model to verify, but the prompt is
the part it can ignore.

## Verification gate

If a turn touched files, it doesn't end on broken code. An auto-detected compile
check runs — `go build ./...`, `cargo check`, `tsc --noEmit`, or `verify_cmd` —
and on failure the model is re-invoked with the errors, up to 4 attempts before
it gives up and tells you.

When no objective check exists for the project, the model is challenged once to
prove it actually verified its work rather than accepting an unevidenced "done".

## Loop guards

Reset each turn:

| Guard | Trigger |
|---|---|
| Step budget | 25 tool rounds (100 in auto). Tools are then disabled and the model must summarize |
| Repeated action | The same call identity N times running — warned, then tools disabled for a round |
| Oscillation | Alternating between the same two actions without progress |
| Re-read | Re-reading a file nothing has changed since it was last read |
| Preamble echo | Re-announcing the same intent in slightly different words before every call |
| Identical failure | The same call with the same arguments failing repeatedly |
| Stream idle | 3 minutes with no output cancels the request |

Inspection calls include their arguments in the repeat identity, so reading
different files is progress; mutation and control tools are matched by name so
varied-argument spam is still caught.

## Context ceiling

`assembleMessages` builds every request under a hard token ceiling derived from
the model's real `num_ctx`, holding back a reserve for generation. History is
included newest-first until the budget is spent.

A tool result is never sent without the assistant tool-call that produced it —
the cut is nudged back past leading tool messages. Anything dropped is
recoverable from the KV archive via `/archive`, and a rolling summary of
compacted history rides along in the volatile tail.

## Secret handling

- The API key field is masked
- A provider can name an environment variable instead of storing a key, and that
  variable outranks anything stored
- The modal says which is winning, so a key typed into an overridden field is
  never silently ignored
- the Cursor agent receives its key through the environment, never argv, so it
  doesn't appear in the process list
- Keys are not accepted on the command line, where they would appear on screen
  and in input history

## Unattended spend

Dream mode — idle background reflection — refuses to run on a routed provider.
It fires while you're away from the keyboard, and a metered API being billed
with nobody watching is not an acceptable default.
