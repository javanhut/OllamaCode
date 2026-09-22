# Eval harness (`cmd/eval`)

`cmd/eval` runs repeatable tool-use regressions against a model: each fixture
gets a fresh temp workspace, the agent loop executes the task prompt with the
real tool registry, and the run is scored on two axes — **behavior** (the
workspace/output check passes) and **tool contract** (required/forbidden/ordered
tool usage). A trial passes only when both do.

## Running against a live model

```sh
go run ./cmd/eval -model ornith:latest                 # every fixture, one trial each
go run ./cmd/eval -model gemma4:8B -task fix-bug       # a single fixture
go run ./cmd/eval -model ornith:latest -constrain      # small-model posture: schema-constrained tool calls
go run ./cmd/eval -model ornith:latest -json > run.json
```

Useful flags:

- `-runs N` / `-samples N` — trials per fixture (`-samples` is an alias that
  overrides `-runs`). Multi-sample runs report a **per-fixture pass rate** in
  the summary and in the JSON `by_task` array, so single-sample noise is
  visible instead of hiding in the global rate.
- `-task NAME` — run only the named fixture.
- `-constrain` — first-pass schema-constrained tool output (native Ollama
  only); the rung fallback cache is shared across all tasks and trials.
- `-legacy-results` — pre-envelope prose tool results, for A/B comparison.
- `-trace PATH` — record a redacted JSONL trace of the run.
- `-min-pass-rate R` / `-min-tool-rate R` (default `1`) — exit non-zero when
  the global behavior or tool-contract rate drops below `R`.
- `-steps N` — agent step cap per task (default 15).

The JSON report aggregates totals (steps, calls, errors, argument failures,
repair attempts/successes, loop-guard blocks, tokens, duration mean/stddev/p95)
plus `by_task` per-fixture pass rates and the raw `results` per trial. Example
outputs from live runs are in `scratch_eval/*.json`.

## Hermetic self-test (`-selftest`)

CI has no Ollama host, so `-selftest` runs the whole fixture set without a
model:

```sh
go run ./cmd/eval -selftest -samples 2
```

Every fixture carries a `Script`: a turn-by-turn playback (tool calls, then a
final answer) served by an in-process scripted `agent.ChatClient`
(`cmd/eval/fakehost.go`). The agent loop, tool execution, behavior checks, tool
contracts, and aggregation all run for real — only the model is fake. This
gates on **fixture self-consistency**: a fixture whose check can never pass,
whose contract contradicts its own script, or that lacks a script fails the
run. The `repair-args` fixture deliberately emits a malformed call so the
argument-repair path (`agent.RepairArgsViaFormat`) is exercised hermetically.

## CI gate

`.github/workflows/ci.yml` runs `go run ./cmd/eval -selftest -samples 2` after
the unit tests. With the default `-min-pass-rate 1` / `-min-tool-rate 1`, any
fixture regression fails the job. No network or Ollama instance is involved.
Live-model comparisons stay a manual, pre-release activity.

## Promoting a trace into a fixture

```sh
go run ./cmd/eval -promote-trace trace.jsonl
```

converts a redacted trace (recorded with `-trace` or from a TUI session) into a
fixture skeleton on stdout — a starting point you edit into a deterministic
`task` in `cmd/eval/main.go`.

## Authoring fixtures

Fixtures are Go literals in `evalTasks()` (`cmd/eval/main.go`): `Name`,
`Prompt`, `Setup` (files to seed), `Tools` (required/optional/forbidden/ordered
expectations), `Filter` (tool allow-list, e.g. read-only), `Check` (behavior
assertion over the workspace and final output), plus `Script` (and optionally
`RepairArgs`) for `-selftest`. Keep them small and deterministic: exact-content
checks, no network, no clock dependence. New fixtures must include a `Script`
or the CI self-test fails.
