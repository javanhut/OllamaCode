# Training: turn traces into fine-tunes

OllamaCode already records every tool call it makes into a redacted JSONL
trace (see `internal/trace`). `cmd/finetune` exports the *successful* tool-call
trajectories from those traces as an instruction dataset, so you can fine-tune
a small local model on how your harness actually works — the ocode tool schema
and result-envelope format, not a generic approximation of them.

A harness that trains its own model.

## Exporting the dataset

```sh
go run ./cmd/finetune                          # default trace -> stdout
go run ./cmd/finetune -trace ~/Library/Caches/ollama_code/trace.jsonl -out dataset.jsonl
go run ./cmd/finetune -trace ./traces/ -out dataset.jsonl -min-calls 2
```

Flags:

- `-trace` — a trace file, or a directory of `*.jsonl` traces
  (default: the TUI's trace path, `os.UserCacheDir()/ollama_code/trace.jsonl`).
- `-out` — output JSONL path (default: stdout).
- `-min-calls` — minimum successful tool calls per record (default `1`).
- `-only-rated` — keep only turns you rated `good` with `/rate` (default off).

Progress and drop statistics go to stderr, e.g.
`42 trajectories: 17 kept, 25 dropped incomplete=9 tool_error=8 argument_failure=5 too_few_calls=3`.

### What gets kept — and what gets dropped

The exporter only trains on clean demonstrations. A trajectory is dropped when:

- **`incomplete`** — a headless run (`agent.Run`, `cmd/eval`, sub-agents) that
  ended with `turn_end` reason `limit_or_loop_guard`, or never ended
  (abandoned). Only `reason: "completed"` qualifies. Interactive TUI turns
  record no completion marker, so for them the tool-event health below is the
  only gate.
- **`tool_error`** — any tool call in the trajectory returned an error.
- **`argument_failure`** — any call needed argument repair, *even if the repair
  succeeded*: after a repair the recorded assistant message may not match the
  call that actually ran, so the example could teach a mismatched call/result
  pair. First-pass-clean calls only.
- **`too_few_calls`** — fewer successful calls than `-min-calls`.
- **`no_prompt`** — the user turn couldn't be recovered from the trace.
- **`rated_bad`** — a human typed `/rate bad` for the turn. Checked ahead of
  every other filter and dropped unconditionally: "it completed" cannot tell a
  right answer from a wrong one, which is the whole reason the rating exists.
- **`unrated`** — `-only-rated` was set and no rating covers the turn. Headless
  traces are unrateable by construction, so `-only-rated` yields TUI turns only.

Redaction is one-way: the exporter passes already-redacted arguments and
results through verbatim and never un-redacts anything. Two gaps in what
traces capture, noted rather than synthesized:

- Interactive (TUI) traces don't record the assistant's final prose answer, so
  records exported from them end at the last tool result. Headless traces
  include the final answer via the recorded `model_response` payloads.
- Headless traces don't record the system prompt, so `system` is empty for
  them; the reference is the system prompt in the repo version that produced
  the trace (`cmd/eval/main.go` for eval runs). Interactive traces carry the
  full system prompt in the recorded request payload, and it is exported.

### Rating turns

`/rate good|bad [note]` in the TUI rates the turn that just completed; bare
`/rate` prints the verdict it currently carries. Ratings are appended to the
same redacted trace as `turn_rating` events carrying
`{turn, from_turn, rating, note}`, where the two generations span every tool
round of the turn — one user turn is a range, because the last generation holds
only the final prose reply and no tool calls. The last rating for a turn wins,
so changing your mind is just typing `/rate` again. Requires `"trace": true` in
the config; with tracing off the command says so rather than dropping the
verdict silently.

Generation numbers restart at 1 in every run and the trace file is appended to
across runs, so both grouping and ratings are scoped to the `session_start`
event each run writes: a verdict never reaches an identically-numbered turn
from a different session.

### Dataset schema

One JSON object per line, chat-format, in the exact envelope shape the model
saw at runtime:

```json
{
  "source": "trace.jsonl",
  "model": "qwen2.5-coder:7b",
  "system": "You are an automated coding agent being evaluated. ...",
  "tools": ["read_file", "edit_file", "run_shell"],
  "messages": [
    {"role": "user", "content": "Fix Add in calc.go so it returns the sum of a and b."},
    {"role": "assistant", "tool_calls": [{"function": {"name": "read_file", "arguments": {"path": "calc.go"}}}]},
    {"role": "tool", "tool_name": "read_file", "content": "{\"ok\":true,\"summary\":\"read_file completed\",\"evidence\":[\"package calc\",\"\",\"func Add(a, b int) int { return a + a }\"]}"},
    {"role": "assistant", "tool_calls": [{"function": {"name": "edit_file", "arguments": {"path": "calc.go", "old_string": "a + a", "new_string": "a + b"}}}]},
    {"role": "tool", "tool_name": "edit_file", "content": "{\"ok\":true,\"summary\":\"edit_file completed\"}"},
    {"role": "assistant", "content": "Fixed Add to return a + b."}
  ]
}
```

- `tools` is the list of tool *names* visible to the model — a stable
  reference to the tool schema. The full JSON schema is versioned with the
  repo (`tools/` registry, `tools.DefaultRegistry().Definitions()`); regenerate
  it from the commit that produced the trace rather than duplicating it into
  every record.
- Each assistant tool-call round is one `assistant` message with `tool_calls`,
  followed by one `tool` message per call whose `content` is the verbatim
  result envelope (`{"ok":...,"summary":...,"evidence":[...]}`) — the same
  string the model read at runtime. Train on it as-is; don't reformat.
- `source` / `model` are provenance; strip or ignore them at training time.

## LoRA recipe

Goal: teach a 7–14B base model ocode's tool-call format and envelope
conventions, not new knowledge. LoRA on an instruct model is the right size of
hammer.

**Base models** (pick one, tool-calling instruct variants):

- `Qwen2.5-Coder-7B-Instruct` or `-14B-Instruct` — best default for a coding harness.
- `Llama-3.1-8B-Instruct` — solid general tool use.
- `Mistral-Nemo-Instruct-2407` (12B) — long-context alternative.

**Framework: axolotl.** Concrete, mainstream, and one YAML away:

```yaml
base_model: Qwen/Qwen2.5-Coder-7B-Instruct
datasets:
  - path: dataset.jsonl
    type: chat_template        # records are already role/content/tool_calls messages
adapter: lora
lora_r: 16
lora_alpha: 32
lora_dropout: 0.05
lora_target_linear: true
sequence_len: 8192             # trajectories carry file contents; don't truncate below 4k
micro_batch_size: 2
gradient_accumulation_steps: 8 # effective batch 16
num_epochs: 3
learning_rate: 1.0e-4
lr_scheduler: cosine
warmup_steps: 20
bf16: true
output_dir: ./ocode-lora
```

Starting points, not gospel: with under ~1k records, prefer 2–3 epochs and
watch for overfitting (the model reciting envelope JSON when it should be
thinking); rank 32/alpha 64 if 16 underfits the tool schema. Merge afterwards
(`axolotl merge-lora`), then convert to GGUF with llama.cpp's
`convert_hf_to_gguf.py` and quantize (`q4_K_M` is a good default).

(Unsloth is an equally fine choice if you want faster iteration on a single
GPU; the same hyperparameters carry over.)

## Serving the result to ocode

```sh
ollama create ocode-ft -f Modelfile   # FROM ./ocode-lora-q4_K_M.gguf
```

Then point ocode at it like any other model — set `model` (or a per-mode
route) to `ocode-ft` in `config.json`, or launch with `OLLAMA_MODEL=ocode-ft`.
See [configuration](configuration.md) and [routing](routing.md). If the
fine-tune is under 15B, ocode's small-model tier applies automatically
(compact prompt, lean toolset); that is the tier these traces were largely
produced under, which is the point.

Validate before adopting: run `go run ./cmd/eval -model ocode-ft -trace
ft-trace.jsonl` and compare pass rate against the base model — and the new
trace is the next training corpus.
