# Model routing

Small local models are good at mechanical edits and bad at deciding what the
edit should be. Routing splits those across two models.

**The mode is the router.** There is no separate task classifier to get wrong —
planning already happens in plan mode and execution in write mode, so binding a
model per mode is all it takes.

```
/route plan openrouter:anthropic/claude-sonnet-4
```

That's the whole setup. An unbound mode falls back to your default model, so
explore and write stay local automatically. Don't bind explore — it does the
bulk file reading and you'd pay for every `read_file`.

```
/route                      # show the table, * marks the current mode
/route write qwen3-coder:30b
/route plan off             # unbind one
/route off                  # disable routing entirely
```

With no routes configured, routing is completely inert — the active model is
never touched.

## What a switch actually swaps

Every mode transition runs `applyRoute`, which re-points three things together:

```
mode changes ──▶ applyRoute()
                   ├── m.modelName   the bound model
                   ├── m.host        its endpoint, wire format, and key
                   └── resolveProfile()   num_ctx, tool support, small-model tier
```

The profile has to move with the model or you assemble a 128k prompt for an 8k
model. Because it re-points `m.modelName` itself rather than just the chat call,
sub-agents, `parallel_edit`, format repair and history compaction all follow the
routed model for free.

## Providers

A route spec is `<model>` on the default host, or `<provider>:<model>` for a
configured endpoint. The prefix only counts when it names a provider that
actually exists — `qwen3-coder:30b` stays a model name, `lmstudio:qwen3-coder:30b`
does not.

Add one with `/provider new`, which opens the connection modal:

```
┌─ Endpoints                                            esc ─┐
│  ↑↓ ‹ + new provider ›  3/3                                │
│                                                            │
│  Name  openrouter                                          │
│  URL   https://openrouter.ai/api/v1                        │
│  Key   ••••••••••••                                        │
│  Env   OPENROUTER_API_KEY                                  │
│  Wire  ‹ openai (/v1/chat/completions) ›  space            │
│  $OPENROUTER_API_KEY is set — it overrides this field      │
│                                                            │
│  tab field  ↑↓ endpoint  enter save & test  ctrl+d delete  │
└────────────────────────────────────────────────────────────┘
```

`enter` saves and immediately lists models from the endpoint you just edited —
a model list coming back is proof the key works.

Keys are never typed on the command line; they go in the masked field, or
better, name an environment variable in **Env** and leave **Key** blank so the
secret never reaches `config.json`.

| | |
|---|---|
| `/provider` | list what's configured |
| `/provider new` | blank slot |
| `/provider <name>` | edit that one |
| `/provider remove <name>` | delete it, and any routes bound to it |

Renaming a provider repoints the routes that referenced it; deleting one clears
them. Otherwise a spec like `old:model` would parse as a plain model name and
silently run on the local daemon.

### Wire formats

Cycle with `space` on the **Wire** row.

**`openai`** — any `/v1/chat/completions` server: OpenRouter, OpenAI, LM Studio,
vLLM, Together, Groq. URL is the base including `/v1`; a URL with no path gets
`/v1` appended.

**`ollama`** — a second Ollama daemon, e.g. a beefier machine on your LAN.

**`cursor`** — the local Cursor agent CLI. Not an HTTP endpoint at all; see
below. URL is the path to the binary, or blank to find it on PATH.

## Offloading planning to the Cursor agent

Cursor publishes no inference API, but it ships a local CLI, so this provider
talks to a subprocess. No proxy, no tunnel, nothing to install but the CLI.

```
/provider new       # Name: cursor, Wire: cursor, URL blank, Env: CURSOR_API_KEY
/route plan cursor:claude-sonnet-4
```

`enter` in the modal runs `--list-models` against it and shows what your account
can use — ids like `auto`, `composer-2.5`, `gpt-5.3-codex-high`,
`claude-opus-5-thinking-high`.

The CLI installs under two names depending on how you got it — `cursor-agent` on
some setups, plain `agent` on others. Leave **URL** blank and both are tried, in
that order, using whichever actually starts. Set it only if the binary lives
somewhere off PATH.

Per turn it runs:

```
<agent> -p --plan --trust \
        --output-format stream-json --stream-partial-output \
        --model <model> --workspace <cwd> "<prompt>"
```

**`--plan` is what makes this read-only**, and it is not optional. Print mode
alone is *not* safe — the CLI's own help says `-p` "Has access to all tools,
including write and shell." `--plan` is documented as "read-only/planning
(analyze, propose plans, no edits)". `--force` and `--yolo` are never passed; a
test fails if either ever appears in the argv.

A headless run also aborts on Cursor's workspace-trust prompt ("Do you trust
the contents of this directory?") unless `--trust` is passed — and that is
**opt-in per provider**, off by default. Turn on the **Trust** row in the
provider modal (it only appears for the cursor kind) to enable it.

Left off, the run fails with an actionable error rather than silently trusting
your repo on your behalf. The alternative is to run `agent` once interactively
in the directory and accept there. Note that the CLI's own suggestion is to pass
`--yolo` or `-f` — do not: those also grant write and shell access and would
undo `--plan`.

The API key goes through the environment, never `--api-key`, so it stays out of
the process list.

### It is an agent, not a model

The Cursor agent reads and searches the workspace itself and speaks no tool
protocol, so three things differ:

- **No tools are sent to it.** Offering them produces fake tool JSON inside its
  prose.
- **It gets a short planning prompt**, not the full tool-protocol system prompt,
  which would be instruction it cannot use and would only imitate.
- **It cannot write its own notes or call `switch_mode`**, so OllamaCode does
  both for it — see the handoff below.

## The full loop

```
you: "refactor the auth layer across all the handlers"
  │
  ├─ cold-start router scores the message ──▶ "This looks like planning work" (y/N)
  │
  ├─ plan mode, on the routed model
  │     the Cursor agent reads the repo, returns a plan
  │
  ├─ plan verified ──▶ does it name real files?
  │     no  → stays in plan mode; your reply goes back to the planner
  │     yes → plan → session notes, mode → write, model → back to local
  │
  └─ Layla executes: each planned file must be read before it is edited,
     every write behind an approval prompt
```

### The cold-start heuristic

The model escalates itself with `switch_mode` once it's running — but on the
first message it isn't running yet, and a small local model is the one least
likely to notice it is out of its depth. So the message is scored before the
turn starts:

| Signal | Weight |
|---|---|
| design phrases — "how should", "best way", "design", "architect", "trade-off" | 2 |
| big verbs — "refactor", "migrate", "audit", "rewrite", "restructure" | 1 |
| breadth — "across", "entire", "whole", "all the", "codebase", "throughout" | 1 |
| ≥3 `@` file references | 1 |
| more than 60 words | 1 |

At **2 or more** it offers the swap. So `refactor this function` scores 1 and
stays local; `refactor the auth layer across all the handlers` scores 3 and
asks.

```
┌─ This looks like planning work            n=stay local ─┐
│  Run it in plan mode on claude-sonnet-4?                │
│  staying local keeps qwen3-coder:30b in explore mode    │
│  matched: refactor, across, all the                     │
│  y/enter switch to plan   n/esc stay local              │
└─────────────────────────────────────────────────────────┘
```

It only ever asks — your y/N is the real classifier, which is why crude scoring
is acceptable. A false positive costs one keystroke. It goes quiet when you're
already in plan or auto mode, when nothing is bound to plan, and after two
declines in a conversation (`/clear` resets that).

The phrase table is `routeSignals` in `tui/route.go` — one place to tune.

### Plan verification

The plan comes from a model that never sees the result of executing it, so it
is treated as a proposal to check rather than an instruction to follow. Two
enforcement points, neither of which depends on the executing model
cooperating:

**Before handoff — is it a plan at all?** Every path the text names is extracted
and stat'd. A text naming no file isn't a plan; it's a question, a refusal, or
an error. Those never reach write mode — it stays in plan mode so your next
message goes back to the planner.

**During execution — read before edit.** The paths the plan named are armed, and
an edit against one is refused until it has actually been read that turn:

> `error: the plan says to change "tui/route.go" but you have not read it this
> turn. Read it first and confirm the plan still matches the actual code, then
> retry edit_file.`

Narrow on purpose: only files the plan itself named, only on a turn that came
from an offloaded plan, and never blocking a file that doesn't exist yet.

The handoff message also carries the findings concretely — "these do NOT exist
yet: `tui/router.go` — either the plan means to create them, or it named the
wrong path." That is the failure this catches best: a plan naming
`tui/router.go` when the file is `tui/route.go`.

## Interactions worth knowing

**`/model use X` while a mode is routed** sets the default, but the route wins
on the next mode switch. The toast says so rather than letting the model appear
to change back on its own.

**Embeddings always run on the default Ollama host**, never a routed provider —
a `/v1` endpoint has no `/api/embed`, and the embed model is local anyway.

**Dream mode won't run on a routed provider.** It fires unattended while you're
idle; spending a metered API with nobody watching is not a thing that should
happen by default.

**OpenAI-compatible endpoints skip profile discovery.** There is no `/api/show`
on `/v1`, so a large tool-capable model is assumed rather than making a
guaranteed-failing round trip on every mode switch. Override with `/model ctx`.
