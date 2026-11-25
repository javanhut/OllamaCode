# Eval harness (`cmd/eval`)

`cmd/eval` runs repeatable tool-use regressions against a model: each fixture
gets a fresh temp workspace, the agent loop executes the task prompt with the
real tool registry, and the run is scored on two axes — **behavior** (the
workspace/output check passes) and **tool contract** (required/forbidden/ordered
tool usage). The two are reported separately; see "Outcome vs tool contract".

## Running against a live model

```sh
go run ./cmd/eval -model ornith:latest                 # every fixture, one trial each
go run ./cmd/eval -model gemma4:8B -task fix-bug       # a single fixture
go run ./cmd/eval -model ornith:latest -constrain      # schema-constrained tool calls (opt-in per model: profile constrained_tool_calls)
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
  the global **outcome** pass rate or the tool-contract rate drops below `R`.
- `-gate-on point|lower-bound` (default `point`) — with `lower-bound`,
  `-min-pass-rate` gates on the lower 95% bound (the smaller of the clustered
  and Wilson bounds) instead of the point estimate, so a lucky small sample
  cannot pass a gate it has not earned.
- `-seed N` — base sampling seed (trial `t` uses `N+t-1`, so trials differ but
  the run is reproducible). `-temperature T`, `-num-ctx N` — decoding options.
  Unset (the default) means server defaults, recorded as such in the metadata.
- `-compare a.json,b.json` — paired comparison of two result files, no model
  needed (below). `-validate-fixtures` — fixture sanity check, no model needed.
- `-steps N` — agent step cap per task (default 15).
- `-verify=false` — turn off the verification gate (on by default, as in
  `ocode -p`), for A/B comparison. Each result records `verification` and
  `verify_failures`.

## Outcome vs tool contract

The headline pass rate is **outcome-based**: did the workspace/answer check
pass (`behavior_passed` per trial; `outcome_passed` / `outcome_pass_rate` in
the report). Tool-contract results (required/forbidden/ordered tools) are a
second, separately reported axis, because grading the path rather than the
result penalizes valid alternative solutions. Keep strict contracts only for
real contracts (read-before-edit, no shell on a read-only task, injection
resistance), not for "the way I would have done it".

For compatibility the older `passed` / `pass_rate` fields keep their meaning
(outcome AND contract) and are printed as "strict pass"; intervals, pass@k,
pass^k, gating and `-compare` all use the outcome axis. The checked-in
`scratch_eval/` runs show why this matters: gemma4 16/21 vs ornith 18/21 was
entirely tool-contract misses (`investigate`, `multi-file` ordering); both
solved 21/21 on outcome.

## Reading the uncertainty block

Every report (text and JSON `stats`) carries:

- **n**: tasks and trials. Trials of one task are correlated, so the number of
  independent draws is closer to the number of *tasks* than trials.
- **Task-clustered 95% CI** (`ci_task_clustered`): mean of per-task pass rates
  ± 1.96 × sd/sqrt(tasks). This is the one to trust. It is undefined for a
  single task and collapses to zero width when every task has the same rate
  (e.g. all 100%); treat a degenerate interval as "no evidence of variance in
  this sample", not as certainty.
- **Wilson 95% CI** (`ci_wilson_trials`): treats all trials as independent, so
  it is optimistically narrow here; shown as a reference.
- **pass^1, pass@k, pass^k** (k = trials per task; `pass_k_curve` has every
  k up to that): unbiased per-task estimators from c successes in n trials,
  pass^k = C(c,k)/C(n,k) (all k trials succeed, the reliability number) and
  pass@k = 1 - C(n-c,k)/C(n,k) (at least one succeeds), averaged over tasks.
  A model at 90% per trial but pass^3 of 60% is not reliable.

## How many trials and tasks

Rule of thumb: for a pass rate near 80%, distinguishing two models 10 points
apart with *unpaired* runs needs about 250 trials per arm (roughly 200-300
independent trials; correlated trials count for less). A 7-fixture x 3-trial
run (21 trials) has a clustered 95% interval of about +/-15-25 points, so
76% vs 86% is noise. Ways to get power cheaply:

- add **tasks** (more independent draws) before adding trials per task;
- use the **paired** comparison below, which cancels per-task difficulty and
  needs far fewer samples than two unpaired intervals;
- run both arms with the same `-seed`, temperature, num_ctx and fixture set.

## Comparing two runs (`-compare`)

```sh
go run ./cmd/eval -model A -samples 5 -seed 1 -json > a.json
go run ./cmd/eval -model B -samples 5 -seed 1 -json > b.json
go run ./cmd/eval -compare a.json,b.json          # add -json for machine output
```

It matches tasks by name and prints per-task pass-rate differences, tasks that
improved or regressed (B vs A), the mean paired difference with a 95%
percentile bootstrap CI over tasks (10,000 resamples, fixed RNG seed so the
output is deterministic), and a verdict. If the interval includes 0 the verdict
is "difference not distinguishable from noise at this sample size"; only when
it excludes 0 does it call a winner. It warns when model, temperature, seed,
num_ctx, fixture hash, harness version or (when both are known) model digest
differ, and when a file predates the metadata block. An empty, corrupt or
result-less file produces a clean error and exit 1 (an interrupted run leaves a
0-byte JSON, e.g. `scratch_eval/gemma4_constrain.json`).

## Reproducibility metadata

Each JSON result has a `metadata` object: model, digest / quantization /
parameter size / family (best effort from Ollama `/api/tags` and `/api/show`;
silently omitted if unavailable), temperature, seed, num_ctx, constrain,
verify, max steps, harness version (`eval-2` plus VCS revision when built from
a checkout), fixture-set hash, Go version, OS/arch and start time. The fixture
hash covers fixture names, prompts, seeded files and tool expectations; check
functions are code and are not hashed, so bump `harnessVersion` when changing
scoring.

## Bash-only baseline (`-toolset bash-only`)

`-toolset bash-only` (default `full`) offers the model only `run_shell`, in the
spirit of mini-SWE-agent, to measure what each custom tool earns per model.
The loop ends on a plain-text answer, so no final-answer tool is needed. The
bash-only filter replaces a fixture's own tool filter (read-only fixtures get
a shell too). Fixtures run normally and are graded on outcome; a fixture whose
contract requires/orders/any-ofs a non-shell tool, or forbids `run_shell`, is
marked `tool_contract_na` and reported as "n/a under bash-only" instead of
failing; n/a trials are excluded from the tool-contract rate (all-n/a = 100%,
so a gate cannot trip). The toolset is in `metadata.toolset`, and `-compare`
warns when toolsets differ (files without the field count as `full`). Not
available with `-selftest` (scripts call non-shell tools).

## Auto-RAG (`-rag on|off`)

`-rag on` (default `off`) A/B tests the TUI's embedding retrieval headlessly.
Per trial it builds a `semantic.Index` of the fresh workspace (embed model via
`-embed-model`, default `nomic-embed-text`, which must be pulled), embeds the
task prompt, takes the top 8 chunks, formats the same token-budgeted
`[RETRIEVED CONTEXT ...]` block as `tui/rag.go` (budget min(num_ctx/8, 6000),
32k assumed when `-num-ctx` is unset) and appends it to the system prompt (the
TUI injects it in its dynamic-context message). Failures degrade silently as in
the TUI; results record `rag_bytes` / `rag_error` per trial and the summary
shows how many trials actually received context. Caveats: eval workspaces are
tiny, so retrieval has little to add (the fixtures mostly cannot show a RAG
benefit); the index is not refreshed as the agent edits (the TUI reindexes
between turns, which a single-prompt run never reaches). The block text comes
from `semantic.FormatBlock`, the same function the TUI uses. Recorded in `metadata.rag` and
`metadata.embed_model`; `-compare` warns when it differs.

## Validating fixtures (`-validate-fixtures`)

```sh
go run ./cmd/eval -validate-fixtures
```

No model needed. For each fixture it checks: unique non-empty name, non-empty
prompt, a `Check`, a `Script`, sane setup paths, no tool both required and
forbidden, and runs the outcome `Check` on the **untouched starting
workspace** with an empty answer; a fixture that passes with no work is
reported `BROKEN` and the command exits 1. What the format does not support:
there is no per-fixture reference *solution* other than `Script`, which
`-selftest` plays and grades; and checks that grade the final answer text are
only exercised here with an empty answer.

## Fixture tiers

The micro-fixtures in `evalTasks()` check single tool-use behaviors (create a
file, read a fact, resist an injected instruction). The coding fixtures in
`cmd/eval/coding_tasks.go` are small Go modules that start broken — fix two
bugs across files, implement a function from its tests, change a signature and
every caller, fix a root cause that lives in a different file than the failing
test points at — and pass only when `go test ./...` passes with the test files
untouched. `TestCodingTasksStartBroken` guarantees none of them passes before
the model acts.

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
`Prompt`, `Setup` (files to seed), `Tools` (required/optional/forbidden/ordered/
any-of expectations — prefer `AnyOf` over `Required` when several tools are
equally valid ways to do the job), `Filter` (tool allow-list, e.g. read-only), `Check` (behavior
assertion over the workspace and final output), plus `Script` (and optionally
`RepairArgs`) for `-selftest`. Keep them small and deterministic: exact-content
checks, no network, no clock dependence. New fixtures must include a `Script`
or the CI self-test fails.
