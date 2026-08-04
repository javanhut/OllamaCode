# Architecture

For people changing the code.

## Packages

| Path | Contents |
|---|---|
| `api/` | LLM transports. `api.go` is Ollama's native `/api/*`; `openai.go` translates `/v1/chat/completions`; `cursor.go` drives the `cursor-agent` subprocess |
| `tools/` | Tool registry, schemas, handlers, JSON salvage, call fingerprinting |
| `tui/` | Everything else — the Bubble Tea model, modes, routing, transcript, modals |
| `internal/agent/` | Headless bounded agent loop, shared by `spawn_subagent` and the eval harness |
| `internal/semantic/` | Embedding index for auto-RAG |
| `internal/memory/`, `internal/storage/` | Cross-session memory, KV archive |
| `internal/safeshell/` | Read-only shell allowlist, VCS bypass interception |
| `internal/session/` | Save and restore |
| `companion/`, `internal/companion/` | Voice popup: STT in, TTS out |
| `cmd/eval/` | Headless evaluation harness |

`tui.Model` is the single Bubble Tea model. It is large on purpose — one state
owner, no cross-component message plumbing.

## One host type, three wire formats

`api.OllamaHost` fronts all three transports. Despite the name it is "an LLM
endpoint"; a rename would churn every call site for no behavioral gain.

```go
host.SetProvider(api.ProviderOpenAI)   // or ProviderOllama, ProviderCursor
```

`ContinuousChat`, `ChatOnce`, `GetModelList` and `GenerateResponse` dispatch on
that field. `ShowModel`, `Embed` and `PullModel` return explicit "not available
here" errors for the non-Ollama kinds rather than 404ing against the wrong path.

Because it is a value type, copying it is safe — goroutines snapshot the host at
spawn time and never race on a mid-turn route change.

### The OpenAI adapter

Three things differ from `/api/chat`, and each corrupts silently rather than
failing loudly:

- **Tool arguments stream as fragments.** `{"pa` … `th":"a.go"}` across chunks,
  keyed by `delta.tool_calls[].index`. Any single fragment is invalid JSON.
  They're accumulated per index and emitted as one terminal chunk shaped like
  `/api/chat`, so the caller's stream loop needs no OpenAI awareness.
- **`tool_call_id` correlation.** OpenAI demands it; Ollama correlates by name
  and position. IDs are synthesized positionally — the Nth pending call is
  answered by the Nth following tool result — skipping the system advisories the
  loop guards splice in between. Unbalanced pairs are dropped rather than sent,
  since providers 400 the whole request and a truncated history window produces
  them.
- **SSE, not newline-delimited JSON.** Comments, `event:` lines, unparseable
  frames and a stream that dies without `[DONE]` are all survivable.

### The cursor transport

`cursor-agent` is a local binary, so this "provider" spawns a subprocess and
parses `--output-format stream-json`. It never passes `--force`, which is what
makes it read-only against your repo. It is an agent rather than a model, so its
profile sets `SupportsTools: false` and it receives a short planning prompt
instead of the full tool-protocol one.

## The turn lifecycle

```
submit()
  ├── append user message, reset per-turn guards
  ├── proactive compaction if history crossed 80% of budget
  ├── cold-start router — may hold the message for a y/N
  └── startStreamWithRAGGate()
        ├── embed the query, inject retrieved code
        └── startStream()
              ├── assembleMessages()   system + newest-fitting history + volatile tail
              ├── toolsForMode()       mode-gated, tier-trimmed
              └── host.ContinuousChat()

  chatChunkMsg      → append to the stream buffer, re-render
  chatToolCallsMsg  → build a pendingBatch, processPendingTools()
  chatDoneMsg       → finish, or hand off an offloaded plan, or verify-gate
```

### Prompt assembly

Ordering is deliberate and stable:

```
[static systemPrompt]  →  [newest-fitting history]  →  [volatile dynamic tail]
```

The static prefix stays byte-stable so KV prefix caching survives across turns.
Everything that varies turn to turn — mode hint, archive summary, retrieved RAG
block, memory, session notes — goes in the tail, never spliced into the prefix.

`historyWindow` includes whole messages newest-first until the budget is spent,
then nudges the cut back past any leading `tool` messages so a result is never
sent without its originating call.

### Tool dispatch

`processPendingTools` walks the batch, and each call passes a preflight before
it runs:

1. mode gating
2. explore-mode shell allowlist
3. VCS bypass interception
4. `switch_mode` checks — auto-mode refusal, redundant no-op, **the plan gate**
5. **read-before-edit** when executing an offloaded plan
6. identical-failure short circuit
7. approval prompt

Rejections here complete synchronously and don't produce a `toolResultMsg`, so
the batch is finalized directly — otherwise a batch made entirely of rejected
calls would hang at `TOOLS n/n`.

### Generation counter

`m.turnGen` increments on every stream start and cancel. Async messages carry
the generation they were produced under and are dropped on mismatch, so a
straggler from a cancelled turn can never write into the new one.

## Routing

`applyModeTransition` in `tui/mode.go` is the single choke point for every real
mode change — `shift+tab`, `/mode`, `/auto`, and the `switch_mode` tool result.
It calls `applyRoute`, which re-points the model, the host and the profile
together.

Routing re-points `m.modelName` itself rather than the chat call, so everything
derived from it follows for free. Routing at the call site would have left
sub-agents and format repair on the previous model.

`applyRoute` is inert when no routes are configured, which is why an
unconfigured install behaves exactly as it did before the feature existed. The
consequence is that removing the *last* route needs `restoreDefaultModel`, or
the model stays stranded on the route just deleted.

## Adding things

**A tool**: write a `tools.Tool` with its schema and handler, register it in
`tools.DefaultRegistry()` (or on the model for tools needing session state), and
add it to `readOnlyToolNames` or `destructiveToolNames` in `tui/mode.go`. Tools
absent from both are unavailable outside write mode.

**A slash command**: handle it in `updateChatKey` in `tui/keys.go` and add an
entry to `slashCommands` in `tui/view.go` — that list drives both autocomplete
and the help screen.

**An input to the settings modal**: add the field, add it to `settingsFields()`,
render it in `settingsModal()`, **and add it to the per-state forwarding tail in
`Update`**. Bracketed paste arrives as `tea.PasteMsg`, which nothing in the type
switch matches, so that tail is the only thing that delivers it. Missing it
drops paste silently — `tui/paste_test.go` exists because that shipped once.

**A provider kind**: add a constant in `api/api.go`, a transport file, dispatch
in the four entry points, and an entry in `providerKinds` in `tui/model.go`.

## Testing

```sh
make test           # go test ./...
make check          # lint + test
```

Tests run with the **package directory as cwd**, so anything touching the
filesystem should build a temp workspace and `t.Chdir` into it rather than
assuming repo-root paths. Anything calling `saveConfig` should set `HOME` and
`XDG_CONFIG_HOME` to temp dirs so it doesn't clobber the real config —
`tui/route_test.go` shows both patterns.

The transports are tested against fakes: `httptest` servers for the OpenAI wire,
and a stub shell script standing in for `cursor-agent` that records its argv.

### Evaluation harness

`cmd/eval` runs scripted coding tasks through the headless agent loop against a
real model, so you can compare models and catch regressions. It reports
pass/fail, step count and timing per task.

```sh
go run ./cmd/eval -model qwen2.5-coder:7b
go run ./cmd/eval -model deepseek-coder-v2 -host http://localhost:11434
```

Requires Ollama with the named model pulled.
