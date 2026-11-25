# Making small/open-weight LLMs reliable at agentic coding (for ocode)

Note: sources below were gathered via search snippets plus two page fetches (Aider unified-diffs page, SWE-agent paper table). Items marked (secondhand) were not verified against the primary page.

## 1. Constrained decoding / grammars for tool calls

### Takeaway
Constraining the output format can hurt free-form reasoning (older evidence, 2024) but JSONSchemaBench reports small gains on downstream tasks when the schema is well covered. The consistent mitigation is to let the model reason in free text first and constrain only the final structured step, which is what ocode's "re-ask under schema after repeated failures" already approximates.

### Cited Findings
- Tam et al. (EMNLP 2024 Industry) report a notable drop in reasoning ability under format restrictions; stricter formats hurt more; JSON mode was usually the worst on GSM8K, Last Letter, Shuffled Objects; relaxed prompts generally did best. Proposed mitigation: reason in natural language, then convert to schema in a second step. (2024 source) — [arXiv 2408.02442](https://arxiv.org/abs/2408.02442v2)
- JSONSchemaBench (10K real-world schemas; Guidance, Outlines, llama.cpp, XGrammar, OpenAI, Gemini): constrained decoding can speed generation up to 50%, improves downstream task performance by up to 4% (even GSM8K), and frameworks differ a lot in schema coverage (best supports ~2x as many schemas as worst). Per-framework numbers not retrieved. — [arXiv 2501.10868](https://arxiv.org/html/2501.10868v3)
- llama.cpp issue reports tool calling degrading at long context when a tool has many optional parameters; a separate report has Qwen3-Coder-Next producing invalid JSON/duplicate fields when writing files. — [llama.cpp #19382](https://github.com/ggml-org/llama.cpp/issues/19382), [llama.cpp #20164](https://github.com/ggml-org/llama.cpp/issues/20164) (search-snippet level)

### Inferences
- Keep the free-text/thinking turn unconstrained and apply schema only on the retry or the argument-extraction step; keep schemas flat with few optional params (each optional param is a failure surface and a coverage risk in some grammar engines).
- Prefer a fallback ladder: native tool call -> salvage -> schema re-ask, as ocode does; consider also applying it to tool-call-as-text models.

### Gaps
- No agentic-coding-specific measurement of constrained vs unconstrained tool calls on small models found. No head-to-head XGrammar/llguidance/Outlines numbers retrieved; llguidance not evaluated in JSONSchemaBench snippets.

## 2. Edit formats

### Takeaway
Format choice matters a lot, and best format is model-dependent: search/replace tends to suit larger models, while udiff variants did better for smaller ones in Diff-XYZ; whole-file is robust but costly. Lenient application is worth a lot (Aider: disabling flexible patching gave 9x more errors).

### Cited Findings
- Aider unified-diff study (GPT-4 Turbo, 89 refactor tasks, 2023-24 and old): SEARCH/REPLACE baseline 20% vs unified diff 61%; lazy comments 12 tasks -> 4. gpt-4-0613: 26% -> 59%. Disabling flexible patch application caused 9x more editing errors; removing high-level-edit prompting caused 30-50% more errors. — [aider unified diffs](https://aider.chat/docs/unified-diffs.html)
- Diff-XYZ (JetBrains, 2025): search-replace works best for larger models; modified udiff variants do better for smaller ones; small open models weak in all formats. — [arXiv 2510.12487](https://arxiv.org/html/2510.12487v2)
- Aider-based comparison with DeepSeek V3/R1: whole-file and search/replace about equal, search/replace slightly fewer tokens; udiff more tokens. — [arXiv 2604.19201](https://arxiv.org/pdf/2604.19201)
- SWE-Edit: Qwen3-8B trained with GRPO to choose between find-replace and whole-file rewrite improved edit success by 12.5 pts; find-replace fails on whitespace mismatch, whole-file robust but expensive on long files. — [arXiv 2604.26102](https://arxiv.org/html/2604.26102)
- Diffs vs whole files paper (Flutter/Dart): diff formats cut tokens but produce more unappliable edits, worse for weaker models (it cites Aider secondhand). — [arXiv 2609.05779](https://arxiv.org/pdf/2609.05779)
- Apply models: Morph claims morph-v3-fast 10,500+ tok/s at 96% accuracy and v3-large 2,500+ tok/s at 98%; its own benchmark shows 46% avg speed gain vs search&replace and (small sample, LLM-judged) 100% vs 84.6% success for grok-code-fast-1. Vendor claims, no independent benchmark found; no Relace speed/accuracy numbers found. — [Morph docs](https://docs.morphllm.com/models/apply.md), [Morph benchmarks](https://www.morphllm.com/benchmarks/fast-apply)

### Inferences
- ocode's tiered matching + syntax refusal matches the evidence (leniency matters). Add: automatic fallback to whole-file rewrite for small files after N failed matches; offer a per-model edit-format setting (udiff-ish for small models, S/R for 30B+).
- An apply-model would be a second model call; for a local setup a small local model could do "apply" but no evidence of quality was found.

### Gaps
- I did not retrieve Aider's live edit-format leaderboard rows for small open models (whole vs diff vs udiff per model); worth checking aider.chat/docs/leaderboards directly.

## 3. Repo maps / context selection

### Takeaway
Cheap, structural, budgeted context (Aider repo map; Agentless hierarchical localization) is competitive with full agents at low cost. Direct embeddings-vs-grep-vs-LSP comparisons for small contexts were not found.

### Cited Findings
- Aider repo map (third-party analyses of source, secondhand): tree-sitter definitions/references -> graph -> PageRank personalized to chat files -> binary search to fit token budget; default budget about max(1024, min(max_input/8, 4096)). — [DeepWiki Aider repo map](https://deepwiki.com/Aider-AI/aider/4.1-repository-mapping), [repo-mapper](https://docs.rs/crate/repo-mapper/latest/source/README.md)
- Agentless: v1 27.33% on SWE-bench Lite at $0.34/issue; v2 32.00% at $0.70/issue; compared at GPT-4o cost to OpenHands $1.14, SWE-agent $1.62. Pipeline: localize (file -> class/function -> edit location) -> repair -> validate. — [arXiv 2407.01489](https://arxiv.org/html/2407.01489v2), [IssueExec](https://arxiv.org/pdf/2607.17286)
- Better file-level localizer plugged into Agentless raised Lite 32.0% -> 38.3%, i.e. localization quality is a main lever. — [BLAgent arXiv 2605.17965](https://arxiv.org/pdf/2605.17965)

### Inferences
- With small windows, a PageRank-ranked symbol map sized to ~1/8 of the context, seeded by files already touched, is a proven design; ocode could combine with LSP document/workspace symbols as the definition source for languages lacking tree-sitter queries.

### Gaps
- No controlled embeddings vs grep vs LSP retrieval comparison found; Aider's own repo-map blog post not retrieved.

## 4. Tool design for weak models (ACI)

### Takeaway
SWE-agent's ablations show interface design moves resolve rate by several points: lint-gated edits, summarized search, ~100-line viewer, and truncated history all helped.

### Cited Findings
- SWE-agent ablation (GPT-4 Turbo, SWE-bench Lite subset, baseline 18.0%): edit without linting 15.0; no edit command 10.3; iterative search 12.0; no search tools 15.7; 30-line viewer 14.3; full-file viewer 12.7; full history 15.0 (vs last-5 observations 18.0); no demonstration 16.3. Empty-output message is described as helpful but unablated. — [SWE-agent arXiv 2405.15793](https://arxiv.org/html/2405.15793) (2024; strong model)
- RAG-MCP: retrieval-filtered tool exposure raised tool-selection accuracy 13.62% -> 43.13% and cut prompt tokens by over 50%; baseline accuracy drops as tool count rises. Model sizes not verified. — [arXiv 2505.03275](https://arxiv.org/abs/2505.03275)
- A blog claims accuracy loss begins at ~10-15 tools but gives no source (treat as unverified). — [MLM guide](https://machinelearningmastery.com/the-complete-guide-to-tool-selection-in-ai-agents/)

### Inferences
- ocode already does LSP-after-edit (the linter analog). Keep observations short, keep only recent N tool results verbatim (summarize older), expose a small tool set, or dynamically expose tools per phase.

### Gaps
- No small-model-specific tool-count study found; no data on description wording or result envelopes.

## 5. Planner/executor splitting

### Takeaway
Aider reports gains from architect/editor with strong reasoner + capable editor, but not universally.

### Cited Findings
- Architect mode: o1-preview architect with DeepSeek or o1-mini editor reached 85% on Aider's code-editing benchmark (SOTA at the time), vs o1-preview alone 79.7%, Sonnet alone 84%. — [aider architect](https://aider.chat/2024/09/26/architect.html), [o1 post](https://aider.chat/2024/09/12/o1.html)
- R1 architect + Sonnet editor scored 64.0% on polyglot at ~14x lower cost than o1; o1+Sonnet did no better than o1 alone. — [aider r1-sonnet](https://aider.chat/2025/01/24/r1-sonnet.html)
- Large-small cascaded code editing paper exists (2026). — [arXiv 2604.19201](https://arxiv.org/pdf/2604.19201) (details not retrieved)

### Inferences
- Splitting pays most when editor is weak at reasoning but good at format; plan text should be concrete (file, symbol, change) so the executor needs minimal inference.

### Gaps
- No evidence on a 4-30B local editor specifically under a large planner.

## 6. Open-weight agentic models and Ollama pitfalls

### Takeaway
Recent open models score 50-70% on SWE-bench Verified under their own scaffolds but tool-call format mismatches across runtimes are a major source of failures; Ollama's context default is a top pitfall.

### Cited Findings
- Devstral Small 2 (24B, Apache 2.0, 256K ctx): 68.0% SWE-bench Verified per Mistral; recommended temperature 0.2. Tool format details not retrieved. — [Mistral](https://mistral.ai/news/devstral-2-vibe-cli/), [Vercel](https://vercel.com/ai-gateway/models/devstral-small-2)
- Qwen3-Coder-30B-A3B: 50.3% Pass@1 in Nebius OpenHands run on a 484-issue subset, with tool-call formatting problems noted; an aggregator lists 60.4% (unverified, not run by them). Qwen3-Coder-Next (80B/3B active) 70.6% (third-party review). — [Nebius](https://nebius.com/blog/posts/openhands-trajectories-with-qwen3-coder-480b), [llmrun](https://llmrun.dev/model/qwen-qwen3-coder-30b-a3b-instruct/benchmarks)
- Qwen3-Coder emits an XML-style tool call format (not Hermes JSON); Hermes-style parsers silently drop calls; an Ollama issue argues Qwen 3.5 is wired to a Hermes JSON pipeline though trained on Qwen3-Coder XML format. — [llama.cpp #15012](https://github.com/ggml-org/llama.cpp/issues/15012), [Ollama #14493](https://github.com/ollama/ollama/issues/14493)
- gpt-oss: needs native Harmony format; reports of tool-call parse failures, runtimes not stopping on the <|call|> token, and no independent reproduction of OpenAI's tool-use scores. — [arXiv 2604.00362](https://arxiv.org/pdf/2604.00362), [mlx-lm #613](https://github.com/ml-explore/mlx-lm/issues/613)
- Ollama silent context truncation: many reports of default 4096 num_ctx truncating prompts (system prompt/tool definitions lost); one source says default now scales with VRAM (4096 below 24 GiB, up to 32768 for 24-48 GiB) - conflicting, verify against current docs. Workaround: set num_ctx explicitly per request or via derived Modelfile. — [Hermes issue](https://github.com/NousResearch/hermes-agent/issues/43900), [OpenClaw docs](https://docs.openclaw.ai/providers/ollama/advanced), [fast.io](https://fast.io/resources/ollama-context-window/)

### Inferences
- ocode should always send options.num_ctx, warn when prompt tokens approach it, and verify (via prompt_eval_count) that the prompt was not truncated. Per-model profiles for tool format and temperature (e.g. 0.2 for Devstral) are justified.

### Gaps
- No verified current Ollama behavior for Qwen3-Coder 30B templates; no Kimi/GLM local-run data; GLM 4.6 listed at 68.2% only on an aggregator.

## 7. Fine-tuning on trajectories

### Takeaway
A few thousand verified trajectories give large gains: SWE-smith's 5k trajectories took Qwen2.5-Coder-32B to 40.2% on Verified; SWE-Gym's 491 gave much less.

### Cited Findings
- SWE-smith: 50k instances from 128 repos; 5,016 Claude 3.7 Sonnet trajectories (from 20k instances) fine-tuned Qwen2.5-Coder-32B to 40.2% (+33.4) on SWE-bench Verified; SWE-Gym-32B (491 trajectories, OpenHands) 20.6 (column assignment likely, not confirmed); 500 trajectories gave 28.2%. — [arXiv 2504.21798](https://arxiv.org/abs/2504.21798)

### Inferences
- ocode could log its own successful trajectories (test-passing, LSP-clean) for later LoRA; even hundreds of in-harness trajectories help match tool formats.

### Gaps
- No LoRA-specific (vs full fine-tune) numbers retrieved; SWE-Gym paper and rejection-sampling papers not directly fetched.
