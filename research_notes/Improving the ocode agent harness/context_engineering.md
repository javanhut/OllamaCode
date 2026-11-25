# Context engineering and compaction for long-running coding agents (applied to ocode)

## 1. What is "context engineering" and which concrete patterns matter?

### Takeaway
Manus and Anthropic converge on the same core patterns: keep the prefix stable and context append-only (KV-cache hit rate), clear or externalize old tool output instead of rewriting history, use files as restorable memory, and keep the plan and errors visible. ocode already has the prefix, spill-to-file and notes pieces. The gaps are mostly in how history is shrunk (clearing vs windowing), plan recitation, and error retention.

### Cited Findings
- Manus calls KV-cache hit rate the most important metric for a production agent. Its input:output ratio is about 100:1, with about 50 tool calls per task. Cached input on Claude Sonnet is $0.30/MTok vs $3/MTok uncached (10x). — [Manus blog](https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus)
- Manus cache rules: stable prefix (no second-precision timestamps at the start of the system prompt), append-only context (never edit past actions or observations), deterministic serialization (JSON key order can silently break the cache), explicit cache breakpoints where required, and prefix caching plus session-ID routing for self-hosted models. — [Manus blog](https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus)
- Manus: do not add or remove tools mid-task, since that invalidates the cache and confuses the model. Mask them instead, using logit masking or response prefill, with shared name prefixes (`browser_`, `shell_`). — [Manus blog](https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus)
- Manus: use the file system as external memory with restorable compression, e.g. drop page content but keep the URL, or drop document content but keep the sandbox path. — [Manus blog](https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus)
- Manus: recite goals by rewriting a todo.md every step. This pushes the plan to the end of context to counter goal drift and lost-in-the-middle over about 50 calls. This is a qualitative claim with no ablation. — [Manus blog](https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus)
- Manus: keep failed actions and stack traces in context so the model updates its beliefs. Also avoid few-shot ruts, because repetitive context makes the agent imitate itself, so add small variation in serialization and phrasing. — [Manus blog](https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus)
- Anthropic lists five techniques: compaction (tune for recall first, then precision; Claude Code keeps decisions, unresolved bugs, and the 5 most recently accessed files), tool result clearing ("one of the safest lightest touch forms of compaction"), structured note-taking, sub-agents returning 1-2k token summaries, and just-in-time retrieval via lightweight references (paths, queries) with hybrid up-front loading of CLAUDE.md. The post gives few numbers. — [Anthropic, Effective context engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)
- Anthropic's context editing (clears stale tool calls and results near the limit) improved an internal agentic-search eval by 29%. Combined with the memory tool, it improved the eval by 39%. On a 100-turn web-search eval it cut token use by 84%. These are vendor internal evals. — [Anthropic/Claude blog](https://claude.com/blog/context-management)
- Cognition: share full agent traces and not only messages, because actions carry implicit decisions. It recommends a dedicated compressor LLM for long histories, describes it as hard to get right, and says Cognition fine-tuned a smaller model for it. — [Cognition](https://cognition.com/blog/dont-build-multi-agents)

### Inferences
- ocode's byte-stable system prompt with a volatile tail matches Manus and the llama.cpp guidance. One thing to check is that the volatile tail (mode hint, RAG, memory, notes) sits after history. This means that whenever it changes, the whole history is re-prefilled on local models, because the tail changes at the end of the prompt. A prefix cache can still reuse everything up to the first changed token, so the cost depends on whether the tail comes before or after the newest turn. Putting it as a trailing block that is stable across consecutive turns when unchanged (or injecting changes only when they differ) would preserve more cache.
- Any history rewriting (windowing that drops old turns, clearing tool results) breaks the cache from the edit point onward. Prefer batching rewrites at compaction boundaries and not every turn. Manus-style append-only plus rare, coarse compaction is the cache-optimal shape. Anthropic's context-editing docs describe clearing in batches for this reason, but I did not verify this; treat it as an inference.
- Plan recitation (a short todo at the tail of context) is cheap on tokens and fits the existing volatile tail and session notes.
- Keeping the error text of failed tool calls, and not trimming it, aligns with Manus. It is also cheap to check in ocode.
- Restorable compression (keep path/URL, drop body) is already what ocode's spill-to-file does. Extending it to old read_file/grep results (replace with a "re-read path X" stub) is the main gap.

### Gaps
- No ablation data was found for todo recitation or keeping errors in context. Both are practitioner claims.
- Anthropic does not publish what the 29%/39% evals measure in detail.

## 2. Compaction and summarization strategies, and masking vs summarization evidence

### Takeaway
The best controlled evidence (JetBrains, "The Complexity Trap") says cheap observation masking matches or beats LLM summarization on SWE-bench Verified at roughly half the cost, and a hybrid is a bit better. Compaction quality is hard to evaluate, and token savings do not equal speed. Context rot research says quality degrades well before the nominal window, which matters most for small local models.

### Cited Findings
- JetBrains Research (Lindenbauer et al., NeurIPS 2025 workshop, arXiv 2508.21433v3, Oct 2025): across five model configs in SWE-agent on SWE-bench Verified, observation masking halves cost vs the raw agent and matches or slightly exceeds LLM-summarization solve rate (about 52% cost cut and +2.6 points solve rate for the best model). Observations are about 84% of an average turn's tokens. A hybrid cuts a further 7% vs masking and 11% vs summarization. OpenHands generalization is described as preliminary. — [arXiv](https://arxiv.org/abs/2508.21433); [search summary](https://arxiv.org/html/2508.21433v3)
- OpenHands SDK: the LLMSummarizingCondenser is the default. Other condensers include NoOp, Rolling, and Pipeline (chains condensers). The older ObservationMaskingCondenser replaces observations outside an attention window (default 5, per a third-party analysis; unverified against current code) with `<MASKED>`. — [OpenHands docs](https://docs.openhands.dev/sdk/arch/condenser); [observation masking condenser](https://docs.all-hands.dev/modules/python/openhands/memory/condenser/impl/observation_masking_condenser)
- Systematic study "Beyond Token Savings" (arXiv 2609.32961): about 35,000 runs across three open-weight models on SWE-bench Verified and Terminal-Bench. On Terminal-Bench with Qwen, policies using about one third of the tokens took 20-80% longer than uncompressed. Policies with similar success solved different tasks, and results were model-dependent. Evaluate on success, latency and cost, and not only on tokens. — [arXiv](https://arxiv.org/abs/2609.32961)
- "Evaluating Context Compaction on the Questions That Come Later" (arXiv 2610.27301): compaction scored on the current question does not reliably predict how it serves later questions. In its MultiRC test, no sentence-selection method beat random sentence deletion on later questions. — [arXiv](https://arxiv.org/abs/2610.27301)
- AutoCompact (arXiv 2610.02163): training an agent to decide when and what to compact (SFT, then RL) gave +9.2 points on SWE-bench Verified and +5.0 on SWE-PolyBench over the base model. It also helped at a 16K window with fallback compaction. This requires training, so it is not directly usable by ocode. — [arXiv](https://arxiv.org/abs/2610.02163)
- CompactionBench (Microsoft Research): in 13.5M Copilot sessions, only 7.8% trigger compaction but they consume 44.2% of tokens. It proposes APEC, which builds indexed summaries asynchronously in the background. I only saw the search snippet. — [MSR](https://microsoft.com/en-us/research/publication/benchmarking-long-horizon-agent-context-compaction-with-compactionbench)
- OPENDEV (arXiv 2603.05344) uses a five-stage adaptive compaction pipeline and reports about 54% less peak observation context. I only saw the search snippet. — [arXiv](https://arxiv.org/pdf/2603.05344)
- opencode (v1): two phases, prune then summarize. Prune walks backwards, protects the most recent about 40k tokens of tool output (PRUNE_PROTECT=40_000), and if more than 20k (PRUNE_MINIMUM) is older it stamps old tool outputs as compacted placeholders. This takes no LLM call. Note that these are third-party write-ups, and opencode V2 docs say prune currently has no runtime effect. — [gist](https://gist.github.com/sam-saffron-jarvis/2c8a6c26297f5748dac55b597381d268); [opencode v2 docs](https://v2.opencode.ai/docs/compaction)
- Codex CLI: compaction replaces history with a model-written "handoff summary" for another LLM (progress/decisions, constraints, remaining work, critical data), injected with a summary prefix to avoid re-summarization loops. The threshold is `model_auto_compact_token_limit` and the prompt is overridable via `compact_prompt`. Sources are third-party and disagree on defaults. A GitHub issue reports mid-task auto-compaction losing context. — [Codex issue #4106](https://github.com/openai/codex/issues/4106); [write-up](https://codex.danielvaughan.com/2026/03/31/codex-cli-context-compaction-architecture/)
- Claude Code: auto-compact trigger is roughly 83.5% of the window with a hardcoded buffer of about 33k tokens (earlier about 45k). This comes from unofficial sources, which conflict with a leaked-code write-up giving a 13k buffer plus a 20k output reserve. — [claudefa.st](https://claudefa.st/blog/guide/mechanics/context-buffer-management); [conflicting source](https://aaronmedina-dev.github.io/claude-code-leak/compaction.html)
- Cline and Goose: not researched in detail. I found no reliable sources in this pass.
- Context rot (Chroma, July 2025, 18 models including Qwen3-8B): performance falls as input grows even on simple tasks. Low needle-question similarity degrades faster. Distractors compound (1 hurts, 4 hurt more). On LongMemEval, about 113k-token full prompts scored significantly below about 300-token focused prompts for all models. Coherent haystacks did worse than shuffled ones. — [Chroma](https://www.trychroma.com/research/context-rot)
- NoLiMa (ICML 2025, Feb 2025, so older): with low literal overlap, 11 of 13 long-context models fall to half of their short-context score by 32K. "Effective length" (85% of base) was 8K for GPT-4o and 1K for Llama 3.1 8B despite 128K claims. — [NoLiMa summary](https://arxiv.org/html/2502.05167v3)

### Inferences
- For ocode, the cheapest large win is probably a deterministic "mask old tool results" tier (opencode-style prune: keep the last N tokens or last K results, replace the rest with a stub plus a path) that runs before any LLM summarization. This costs no local-model inference, which is slow, and JetBrains evidence suggests quality loss is small. Summarization then becomes a second tier, and the hybrid evidence supports that order.
- Local models are small, so the Chroma and NoLiMa results suggest effective context is much smaller than the num_ctx. Compacting at 80% of an 8B model's 32k window may be too late for quality, even if it is right for fit. Consider a quality-based soft threshold (e.g. mask at 40-50%, summarize at 70-80%), with the thresholds tunable per model. This is an inference, not a measured result for ocode.
- Summaries should be evaluated on later tasks, not only the current one (2610.27301), so keep the structured items the next steps will need (file paths, failing tests, decisions, open errors) and not generic prose.
- Compaction that is slow on a local model can cost more wall time than it saves (2609.32961), so measure latency, not tokens.

### Gaps
- Exact Claude Code compaction prompt and current defaults, Cline, and Goose behavior: not verified from primary sources.
- JetBrains blog returned 403, and the window sizes used in the masking experiments were not retrieved.
- All masking-vs-summary results are on large API models, not 7-30B local models.

## 3. Subagents for context isolation

### Takeaway
Subagents help when work is parallel, read-heavy and breadth-first (research), and each returns a condensed result. They hurt in tightly coupled coding work where decisions must be shared, and they cost many more tokens.

### Cited Findings
- Anthropic's multi-agent research system (Opus 4 lead, Sonnet 4 subagents) beat single-agent Opus 4 by 90.2% on an internal research eval. Token usage explained 80% of BrowseComp performance variance. Agents use about 4x tokens vs chat and multi-agent about 15x. — [Anthropic](https://www.anthropic.com/engineering/multi-agent-research-system)
- Same post: poor fit for tasks where agents must share context or have many dependencies, and for most coding tasks (fewer parallelizable parts). Subagents write outputs to external storage and return lightweight references to avoid copying through the coordinator. The lead saves its plan to memory because context past 200k gets truncated. — [Anthropic](https://www.anthropic.com/engineering/multi-agent-research-system)
- Cognition ("Don't build multi-agents"): principles are to share full traces and to recognize that actions carry implicit decisions. Its examples were a Flappy Bird clone with mismatched subagent outputs and a two-model edit pipeline that misread instructions. It prefers a single linear agent with a compression model. Written in 2025 (older than the 2026 preference). — [Cognition](https://cognition.com/blog/dont-build-multi-agents)
- Anthropic context post: subagents may use tens of thousands of tokens but return 1-2k token summaries. — [Anthropic](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)

### Inferences
- The two sources are consistent when subagents are used as read-only, context-isolating explorers (search the repo, summarize logs, run tests) and the main agent keeps all write decisions. For ocode this suggests subagents should default to read-only investigation with a bounded structured result, with output written to a file and a path returned for large results.
- On a local single-GPU Ollama setup, parallel subagents usually serialize on one model, so the benefit is context isolation only, and not speed.
- The Anthropic 90.2% figure is for research and not coding, and is vendor internal, so do not extrapolate to coding.

### Gaps
- No independent measured evaluation of subagents for coding tasks was found.

## 4. Memory systems for coding agents and measured value

### Takeaway
Measured evidence that repo context files (AGENTS.md style) help is weak: LLM-generated files do not help and add cost; short human-written ones help slightly. Memory tools plus context editing show vendor-reported gains on long tasks.

### Cited Findings
- ETH Zurich (Gloaguen et al., arXiv 2602.11988, ICLR 2026): LLM-generated context files changed success by about -0.5% (SWE-bench) and -2% (AGENTbench), non-significant, while raising cost about 20-23% (significant). Developer-written files gave about +2.4% (non-significant) with up to 19% more cost, and files encouraged more exploration and steps. Recommendation: omit LLM-generated context files, and keep human-written ones to information not already in the README (specific conventions, non-functional requirements). — [arXiv](https://arxiv.org/html/2602.11988)
- Lulla et al. (arXiv 2601.20404) measured Codex runtime and tokens with and without AGENTS.md across 10 repos and 124 PRs. I did not retrieve the headline results. — [arXiv](https://arxiv.org/html/2601.20404)
- Anthropic memory tool plus context editing: +39% on an internal agentic-search eval (+29% for editing alone). The memory tool is client-side file CRUD in a persisted directory. — [Claude blog](https://claude.com/blog/context-management)
- Anthropic's Pokemon example showed note-taking across context resets (tallies, maps, combat notes). It was an anecdote. — [Anthropic](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)
- "Code as Agent Harness" (arXiv 2605.18747), a position paper, argues memory gains in code agents depend on reliable evaluation, because contamination and weak tests can inflate reported gains. I only saw the snippet. — [arXiv](https://arxiv.org/pdf/2605.18747)

### Inferences
- ocode's per-workspace long-term memory and dream-mode consolidation should be kept short and non-redundant with the README and code. The ETH data suggests that auto-generated memory that restates what the agent can read from the repo adds cost without benefit. Dream mode should prefer pruning and deduplication, and favor entries about non-obvious conventions, commands that failed, and user preferences.
- Memory should be injected under a token cap, and its marginal value should be A/B tested (with and without) on a fixed task set.

### Gaps
- No measured evaluation of memory for local small models. The agentic-memory research papers were not surveyed in depth in this pass.

## 5. Guidance for small context windows and local models

### Takeaway
The main local-model risks are Ollama silently running a small default num_ctx, prefix-cache invalidation from prompt changes, and effective context being much shorter than nominal.

### Cited Findings
- Ollama default context depends on VRAM: under 24 GiB is 4k, 24-48 GiB is 32k, 48 GiB and above is 256k. Set with `OLLAMA_CONTEXT_LENGTH`. Agents and coding tools should use at least 64k. Check `ollama ps` to confirm 100% GPU and the allocated context. The page does not describe truncation behavior. — [Ollama docs](https://docs.ollama.com/context-length)
- Other guides (secondary, and they conflict on defaults of 2048 vs 4096): KV cache is allocated for the full num_ctx at load. `OLLAMA_KV_CACHE_TYPE` supports f16 (default), q8_0, q4_0, and needs flash attention (`OLLAMA_FLASH_ATTENTION=1`). — [SSD Nodes guide](https://www.ssdnodes.com/learn/ollama-kv-cache-quantization)
- llama.cpp prefix reuse is based on comparing token prefixes. Changing system instructions, tool declarations, template output, or earlier message serialization breaks reuse from that point. Appending a tool result at the end does not. Requests must hit the same slot; `--cache-ram` and `--cache-idle-slots` help restore after idle. Check `cache_prompt` and compare token IDs across turns. Hybrid or recurrent models can force full reprocessing. — [Schmalbach](https://www.vincentschmalbach.com/why-llamacpp-reprocesses-the-whole-prompt-after-every-tool-call-and-how-to-keep-the-kv-cache-warm/)
- AutoCompact tested a 16K window, where overflow triggered fallback compaction and the method still helped. — [arXiv](https://arxiv.org/abs/2610.02163)
- Chroma includes Qwen3-8B, which shows invented tokens in repeated-word replication from about 5,000 words. NoLiMa found Llama 3.1 8B has about 1K effective length on its benchmark. — [Chroma](https://www.trychroma.com/research/context-rot); [NoLiMa](https://arxiv.org/html/2502.05167v3)

### Inferences
- ocode should detect the actual loaded context (`ollama ps` or the API show/ps endpoints) and set its budget from that rather than from the model card. It should warn if it is below about 16k-64k. I did not verify the API fields; check them.
- Keep tool schemas fixed within a session (Manus). Where tools differ per mode, prefer masking (reject or flag at call time) to changing the tool list.
- Token budgeting: reserve explicit slices for system prompt plus tools, volatile tail (RAG, memory, notes), output, and history. Cap each tail component. On an 8k window the static prompt and tool schemas alone may consume a large share, so a "lite" tool set may be needed.
- Beware that truncation in Ollama when the prompt exceeds num_ctx may silently drop content; I found no authoritative documentation on it, so test it and make ocode's own budget guard conservative.

### Gaps
- No authoritative source on Ollama's truncation or shifting behavior, or its cache invalidation rules, was found.
- Ollama-specific prefix caching behavior (vs llama.cpp server) was only found in a secondary source.
- The quantized KV cache quality impact on coding tasks was not researched.
