# Evaluation methodology for coding agents and agent harnesses

Note on coverage: roughly 19 tool calls. Search-result summaries were used for many items, and only the Anthropic evals post, OpenAI's SWE-bench Verified post, the error-bars paper and "On Randomness in Agentic Evals" were fetched in full. Items marked (secondary) or (unverified) should be checked at the original source before being quoted. Several arXiv IDs (26xx.xxxxx) were surfaced by search and not opened. I found no sources on LiveCodeBench, Multi-SWE-bench, SWE-bench Multimodal/Multilingual/Live, SWE-Gym or R2E details, mining tasks from git history, or how Cline, OpenHands, Codex and opencode evaluate harness changes. These are listed under Gaps.

## 1. Benchmarks in 2025-2026: which are contaminated or saturated, which are cheap for local models, which measure the harness

### Takeaway
SWE-bench Verified is saturated and contaminated, and OpenAI has stopped reporting it. SWE-bench Pro was recommended as the replacement and then withdrawn after an audit found about 30% of its tasks broken. Terminal-Bench 2.x is the most harness-sensitive public benchmark, but it is also reward-hackable and too hard for small local models (about 15% for smaller models). For a small Ollama harness, the useful public sets are the ones that are cheap and not yet saturated for small models: Aider polyglot, SWE-rebench (fresh tasks), BFCL (tool-call format) and tau-bench (pass^k reliability). Treat any of them as a secondary signal beside a custom suite.

### Cited Findings
**SWE-bench Verified**
- OpenAI (2026-02-23) stopped reporting SWE-bench Verified and urged others to do the same. Verified has 500 problems curated from 1,699 reviewed by three experts each. Frontier scores rose from 74.9% to 80.9% in the prior six months. — [OpenAI](https://openai.com/index/why-we-no-longer-evaluate-swe-bench-verified/)
- OpenAI audited the 138 problems o3 did not consistently solve across 64 runs, each reviewed by at least six engineers. 59.4% had material test or description issues: 35.5% narrow tests (reject valid fixes), 18.8% wide tests (test behavior the issue never specifies), 5.1% other. Examples: pylint-dev__pylint-4551 and sympy__sympy-18199. — [OpenAI](https://openai.com/index/why-we-no-longer-evaluate-swe-bench-verified/)
- Contamination evidence: GPT-5.2 solved 31 tasks considered near-impossible, and its chain of thought referenced release-note knowledge. Red-teaming showed GPT-5.2-Chat, Claude Opus 4.5 and Gemini 3 Flash Preview could reproduce gold patches or verbatim problem details for some tasks. — [OpenAI](https://openai.com/index/why-we-no-longer-evaluate-swe-bench-verified/)
- Anthropic says Verified is "approaching saturation above 80%". LLM scores rose from about 40% to over 80% in one year. — [Anthropic, Demystifying evals](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)
- Epoch AI rates Verified "Flawed" on contamination grounds (partial review; secondary). — [Epoch review](https://epoch.ai/benchmarks/swe-bench-verified/review)

**SWE-bench Pro**
- Design: Scale AI, 1,865 tasks total, 731 public. The commercial set has 276 instances from 18 startup repos with private codebases. The public and held-out sets use strong copyleft (GPL) repos to deter training. v1 paper: at most 23.3% on public and at most 17.8% on commercial. These are v1 numbers and may have changed. — [Scale paper v1](https://arxiv.org/abs/2509.16941v1); [Epoch review](https://epoch.ai/benchmarks/swe-bench-pro/review)
- OpenAI's follow-up audit flagged 200 tasks (27.4%) by automated pipeline and 249 (34.1%) by human annotators, about 30% broken. OpenAI withdrew its earlier recommendation. The audit covered only the public set. — [OpenAI, Separating signal from noise](https://openai.com/blog/separating-signal-from-noise-coding-evaluations) (via search summary; the retraction date of 2026-07-08 comes only from [tech-insider.org](https://tech-insider.org/ie/openai-swe-bench-pro-retraction-2026/), which is of unclear quality)
- Independent LessWrong audit (2026-02-24, self-published, less rigorous) found issues in 83 of 100 sampled tasks, mainly "requirements inflation". Epoch rates Pro "Flawed". — [LessWrong](https://www.lesswrong.com/posts/nAMhbz5sfpcynjPP5/swe-bench-pro-is-even-worse); [Epoch](https://epoch.ai/benchmarks/swe-bench-pro/review)
- CAIS found the official Harbor SWE-Bench Pro registry images may stay unpatched and exploitable. — [CAIS blog](https://safe.ai/blog/evaluating-ai-progress-on-swe-bench-pro)

**SWE-rebench**
- Nebius. 21,336 verifiable tasks from 3,468 repos, collected by an automated, continuously running pipeline. The leaderboard uses fresh tasks to avoid contamination. Dataset on Hugging Face as nebius/SWE-rebench. — [arXiv 2505.20411](https://arxiv.org/abs/2505.20411)
- Single independent study (Waterloo, arXiv 2512.10218): models scored about 3x higher on SWE-bench Verified than on SWE-rebench for comparable Python projects. — [arXiv 2512.10218](https://www.arxiv.org/pdf/2512.10218) (search summary)

**Terminal-Bench 2.0 / Harbor**
- TB 2.0 has 89 tasks, each manually checked by three reviewers. It runs on the Harbor harness. Frontier agents resolve under 65% of tasks and smaller models around 15%. — [TB 2.0 paper](https://arxiv.org/html/2601.11868v1); [tbench docs](https://tbench.ai/docs)
- TB 2.1 modifies 26 tasks (bugs, timeouts, resources, reward-hack robustness). Another paper says 28 tasks were revised. These counts conflict across sources. — [terminal-bench-2-1 repo](https://github.com/harbor-framework/terminal-bench-2-1); [arXiv 2608.22103](https://arxiv.org/pdf/2608.22103)
- Reward hacking audits: 409 of 1,860 tasks (22%) hackable across five terminal-agent benchmarks ([arXiv 2610.15924](https://arxiv.org/pdf/2610.15924)); 323 of 1,968 (16%) hackable from the task description alone ([arXiv 2606.08960](https://arxiv.org/html/2606.08960)). Berkeley RDI claims a 100% score on all 89 TB tasks by tampering with test infrastructure ([RDI blog](https://rdi.berkeley.edu/blog/trustworthy-benchmarks-cont/)). These are adversarial demos from different methods and are not comparable.
- A LessWrong LLM-audit of TB 2.0 flagged 15 tasks rated 3 or lower; two of those were false positives on manual review (individual blog, low rigor). — [LessWrong](https://www.lesswrong.com/posts/HzjssjeQqhf3kRw9r/every-benchmark-is-broken)

**Harness vs model**
- On the TB 2.0 leaderboard (observational, not an ablation), GPT-5.3-Codex scores 64.7% under Terminus 2 and 78.4% under SageAgent. Submissions may differ in prompts, budgets and versions. — [survey arXiv 2606.20683](https://arxiv.org/pdf/2606.20683) (search summary)
- Another paper reports the same model going from 69.7% to 77.0% pass@1 with only the harness changed. — [arXiv 2605.23950](https://arxiv.org/html/2605.23950) (search summary)
- "How good is your harness?" (ICML 2026) reports gains from picking the best harness are comparable to gains from picking the best LLM. "The Scaffold Effect in Coding Agents" (ICML 2026) used a 50-task TB subset and found large harness effects. — [ICML 2026 68323](https://icml.cc/virtual/2026/68323); [ICML 2026 82739](https://icml.cc/virtual/2026/82739)
- "What Does a Harness Buy? Tokens, Mostly" compares pinned Claude Code, mini-SWE-agent and OpenCode on SWE-bench Verified. One example lists 52.1% vs 57.8% on the same model, with the lower scorer using 65x the input tokens. — [arXiv 2610.04433](https://arxiv.org/pdf/2610.04433) (search summary)
- Hugging Face's coding-agent leaderboard found DeepSeek's harness tied first overall but sixth on TB, because it kills background processes at task end. — [The New Stack](https://thenewstack.io/deepseek-harness-cleanup-benchmark/)

**Aider polyglot**
- 225 hardest Exercism exercises across six languages (from 697), replacing the saturated 133-exercise Python set. The metric is pass_rate_2: all hidden unit tests pass within two attempts. — [Aider](https://aider.chat/2024/12/21/polyglot.html); [Epoch](https://epoch.ai/benchmarks/aider-polyglot)
- Top-of-board figures conflict (88.0% gpt-5 high vs 89.4% Claude Opus 4.5) and are unverified. Check [aider.chat leaderboards](https://aider.chat/docs/leaderboards). No source confirmed Ollama results.

**tau-bench and BFCL**
- tau-bench (ICLR 2025) introduced pass^k. gpt-4o scored about 61% pass^1 on retail and about 35% on airline. Retail pass^8 fell to about 25%. — [tau-bench](https://arxiv.org/abs/2406.12045)
- BFCL v4 is weighted 10% non-live single-turn, 10% live single-turn, 10% irrelevance, 30% multi-turn, 40% agentic (web search, memory, format sensitivity). Epoch rates it "Flawed" (no agentic environment, excessive involuntary termination). — [Berkeley leaderboard](https://gorilla.cs.berkeley.edu/leaderboard); [Epoch](https://epoch.ai/benchmarks/berkeley-function-calling-leaderboard/review)

### Inferences
- Verified, Pro and Terminal-Bench are poor targets for a small local-model harness. Verified and Pro are contaminated or broken, and TB gives near-floor scores for small models (about 15%), so deltas vanish into noise. Aider polyglot (225 tasks, cheap unit-test grading), a SWE-rebench slice, and BFCL-style tool-format checks fit better. Using them on local models is my judgment, not a documented practice.
- Harness effects of 5-14 points on the same model show that harness-level evals are valuable. They also show that cross-paper comparisons need pinned harness, token budget and version.
- Because of the hackability findings, any imported benchmark should run in a sandbox where agents cannot read test files.

### Gaps
- No primary sources found for LiveCodeBench, Multi-SWE-bench, SWE-bench Multimodal, Multilingual or Live, or Terminal-Bench 2 per-model scores for local or open-weight models.
- No Ollama-specific benchmark results found.
- The retraction date of SWE-bench Pro is single-sourced from a weak site.

## 2. Statistical rigor: variance, trials, confidence intervals, paired comparisons, pass@k vs pass^k

### Takeaway
At ocode's current scale (7 tasks x 3 trials), a 76% vs 86% gap is well inside noise. Agentic pass@1 has a run-to-run standard deviation of roughly 0.7-1.8 points on 500 tasks, and larger on a small suite. Use paired per-task comparisons, many more tasks, repeated trials per task, a resampling or clustered CI, and report pass^k alongside pass@1.

### Cited Findings
**Error bars paper (Anthropic / Evan Miller, Nov 2024; older but the standard reference)**
- 95% CI = mean +/- 1.96 x SE. For binary scores SE = sqrt(p(1-p)/n). Report n and SE beside each score. — [arXiv 2411.00640](https://arxiv.org/html/2411.00640v1); [Anthropic post](https://www.anthropic.com/engineering/statistical-approach-to-model-evals)
- Unpaired difference SE = sqrt(SE_A^2 + SE_B^2). Paired SE = sqrt(Var(s_A - s_B)/n). Question scores across frontier models correlate 0.3-0.7, so pairing reduces variance. With correlation 0.5, variance drops by one third in the paper's example. — same sources
- Clustered questions inflate SE: clustered/naive was 3.05x for DROP, 1.88x for MGSM and 1.10x for RACE-H. Tasks with multiple trials form clusters. — [arXiv 2411.00640](https://arxiv.org/html/2411.00640v1)
- Power: n = (z_alpha/2 + z_beta)^2 (omega^2 + sigma_A^2/K_A + sigma_B^2/K_B) / delta^2. Example: detecting a 3-point difference at 80% power and alpha 0.05 needs about 969 questions. They recommend at least 1,000 questions for new evals. — same paper
- Resampling K answers per question helps only up to a limit: variance cut by 1/3 at K=2, 1/2 at K=4, 5/9 at K=6, ceiling 2/3. With n=198, MDE drops from 13.2% to 7.5% moving from K=1 to K=10. — same paper
- Do not lower temperature to reduce variance. Compute the SE across question-level means, not by pooling all K x N answers. — same paper

**Run-to-run variance in agent evals ("On Randomness in Agentic Evals", 2026)**
- SWE-bench Verified (500 tasks), Qwen3-32B, DeepSWE-preview and Devstral-2 (123B), scaffolds nano-agent and R2E-Gym. 10 runs per configuration, about 60,000 trajectories. — [arXiv 2602.07150](https://arxiv.org/html/2602.07150v2)
- Single-run pass@1 spans 2.2-6.0 points across runs. Standard deviation is 0.7-1.8 points, above 1.5 points even at temperature 0. Example: DeepSWE-preview/nano-agent at temperature 1.0 is 31.4% +/- 1.0, with runs from 28.8% to 32.4%. — same
- Temperature 0 is not deterministic. First-token divergence occurs around token 56 at T=0 and the first 1% of the trajectory. DeepSWE-preview scored 31.4% at T=1.0 vs 20.4% at T=0 (an 11-point gap), so temperature is a big effect for some models. Devstral-2 showed no significant difference between T=0.2 and T=0 (63.5% vs 63.8%). — same
- Runs needed (80% power, p<0.05, two-sample t): at sigma 1.5, 1 point needs about 36 runs, 2 points about 9, 5 points 2. At sigma 1.8, 1 point needs 51 and 2 points 13. At sigma 0.7, 1 point needs 8 and 2 points 2. Their conclusion is that 10 runs detect only differences of about 2 points or more, and 2-3 point gains may be noise. — same
- Metric gap example (DeepSWE-preview / R2E-Gym): pass@1 34.4%, pass@5 52.9%, pass^5 15.5%. They recommend reporting pass@1 mean and std, pass@k as an optimistic bound and pass^k as a pessimistic bound. — same
- Another paper reports a 0.37-point std over five runs for SWE-Agent + MiniMax M2.5, so variance is setup-specific. — [Chronos, arXiv 2610.11578](https://arxiv.org/pdf/2610.11578) (search summary)

**pass@k vs pass^k**
- pass@k = chance of at least one success in k tries, rising with k. pass^k = chance all k trials succeed, falling with k. A 75% per-trial rate gives 0.75^3 = about 42% for pass^3. Use pass@k when one success is enough, and pass^k when consistency matters. — [Anthropic](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)
- pass^k introduced in tau-bench. gpt-4o's retail pass^8 is about 25% from about 61% pass^1. — [tau-bench](https://arxiv.org/abs/2406.12045)
- Anthropic treats near-0% across many trials (e.g. 0% pass@100) as a broken task or grader, not an incapable agent. — [Anthropic](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)

### Inferences
My own arithmetic (not from a source), assuming 21 independent Bernoulli trials and binary pass/fail:
- 76% is 16/21 and 86% is 18/21. SE is about 9.3 points and about 7.6 points. The 95% CIs are about +/-18 and +/-15 points. Unpaired SE of the difference is about 12 points, z about 0.8, so the gap is not significant. This ignores that 3 trials of the same task are correlated, so the real uncertainty is larger (effective n is closer to 7 tasks).
- Unpaired MDE at 80% power with 21 trials per arm is about 34 points. To detect 10 points near p=0.8 unpaired takes roughly 250 trials per arm. Pairing by task and using more tasks lowers this. A 10-point effect on a 50-task x 5-trial suite is still borderline unpaired.
- Practical design for ocode: grow to 50-100+ tasks (fixtures can be graded with execution), run K=3-5 trials per task, compare variants on the same tasks and seeds, and bootstrap over tasks (cluster-level). Report per-task flip counts (McNemar-style) next to pass rates. For CI gates, gate on a CI bound or a large margin, not a point estimate.
- Tool-call contract fixtures are largely deterministic checks. Separating them from outcome tasks is sensible, since contract tests with a stable base rate near 100% behave as regression tests and can be gated at low trial counts.
- Constrained vs unconstrained decoding is a temperature-like confound: the T=1.0 vs T=0 spread of 11 points for one model shows these settings can move results more than a model swap, so pin and report them.

### Gaps
- No source quantified paired-comparison or McNemar guidance specific to agent evals; the paired formula is from the error-bars paper only.
- No empirical variance numbers for Ollama local models at small quantizations.

## 3. Building custom eval suites: synthetic generation, mining history, graders, trace-based regression

### Takeaway
Start with 20-50 tasks from real failures, give each a reference solution, grade by outcome with execution-based tests, use LLM judges only for what tests cannot check, and read transcripts. SWE-smith and SWE-rebench show how to scale generation, but synthetic or mined tasks need validation.

### Cited Findings
- Anthropic: "20-50 simple tasks drawn from real failures is a great start". Source tasks from manual pre-release checks, bug trackers and support queues. Two domain experts should reach the same verdict. Each task needs a reference solution that passes all graders. — [Anthropic](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)
- Anthropic: grade outcome (final state) over exact step sequence, since rigid step-order checks penalize valid solutions. Use deterministic graders where possible and LLM graders where needed. Give LLM judges an "Unknown" option, score each dimension with a separate judge, and calibrate against humans. Add partial credit. Add tool-call verification and metrics for turns, tool calls, tokens and latency. — [Anthropic](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)
- Anthropic: balanced sets should test when behavior should and should not happen. Start each trial from a clean environment, because leftover files or git history leak information. Capability evals start low and regression evals sit near 100%, and saturated capability evals migrate into regression. — same
- Grader bugs matter: Opus 4.5 scored 42% on CORE-Bench until rigid grading (rejecting "96.12" vs "96.124991") and ambiguous specs were fixed, after which it scored 95%. — same
- SWE-smith (NeurIPS 2025 D&B): build the execution environment per repo first, then synthesize bugs (LM-generated or procedural: remove a conditional, change an operator). Keep only tasks that break at least one unit test, then generate issue text. 50k instances from 128 repos, MIT license. Qwen 2.5 Coder 32B fine-tuned on about 5,000 trajectories reached 40.2% pass@1 on Verified. — [arXiv 2504.21798](https://arxiv.org/abs/2504.21798); [GitHub](https://github.com/SWE-bench/SWE-smith)
- SWE-rebench: automated pipeline collecting verifiable tasks from GitHub continuously (21,336 tasks, 3,468 repos). — [arXiv 2505.20411](https://arxiv.org/abs/2505.20411)
- LLM judge reliability on coding trajectories: one harness-design study reported about 94% raw agreement and weighted kappa 0.929 against three human annotators on 200 trajectories, with lower agreement (79.8%-97.3%) on Terminal-Bench action labels. Agent-as-a-Judge (Oct 2024, older) reported about 90% agreement with human consensus vs about 70% for a plain LLM judge. OSWorld-Pro kappas ranged 0.85-0.99 by aspect. All are single papers on 50-200 trajectories. — [arXiv 2609.20804](https://arxiv.org/pdf/2609.20804); [Agent-as-a-Judge](https://arxiv.org/abs/2410.10934); [OSWorld-Pro](https://arxiv.org/pdf/2609.24890) (search summaries)
- TRACE (search summary): LLM judges diverge from humans in realistic coding workflows, for example favoring longer explanations. — [arXiv 2603.24586](https://arxiv.org/html/2603.24586)
- Task-authoring guide drawn from Terminal Bench work lists bad-task failure modes and cites over 15% of tasks in popular terminal benchmarks as reward-hackable. — [arXiv 2604.28093](https://arxiv.org/abs/2604.28093)

### Inferences
- ocode's split between tool-call checks and workspace-outcome checks matches Anthropic's advice, but "ordered tool calls" is exactly the rigid step check Anthropic warns against. Keep ordering only for true contracts (e.g. read before edit) and make everything else outcome-based.
- Mining the repo's own git history (commit with a test change plus a source change, check out the parent, require the new test to fail before and pass after) is a natural SWE-rebench- or SWE-smith-style source for ocode. This is my synthesis; no source describes this for a small repo.
- Replaying redacted traces and promoting failed traces to fixtures already matches "start from real failures". Add a reference solution check per fixture and a validity check (the task fails on the base, passes with the fix).
- Use programmatic mutation tasks (SWE-smith style) for cheap volume. They tend to be easier than real issues, so they suit regression rather than headline capability numbers.

### Gaps
- No primary source found on SWE-Gym or R2E-Gym construction details, or on mining a repo's own git history.
- No data on whether LLM judges run by small local models are reliable; the cited agreement numbers come from frontier judges.

## 4. How harness developers evaluate harness changes in practice

### Takeaway
Evidence found is thin and mostly from Aider and Anthropic. Aider tests harness-level choices (edit formats) against a fixed Exercism benchmark. Anthropic recommends eval-driven development with small suites, transcript reading and production monitoring.

### Cited Findings
- Aider's benchmark has been used to choose edit formats. JSON-wrapped code produced lower-quality code than plain text across several top models (Aug 2024, older); unified diffs reduced GPT-4 Turbo laziness from 20% to 61% on Aider's laziness benchmark (2023, older); function-call edits did worse than whole-file output in the original benchmark. — [Aider code-in-json](https://aider.chat/2024/08/14/code-in-json.html); [unified diffs](https://aider.chat/docs/unified-diffs.html); [benchmarks](https://aider.chat/docs/benchmarks.html)
- Anthropic recommends eval-driven development, written before the capability exists, with ownership, plus production monitoring, A/B tests, user feedback, transcript reviews and periodic human studies. — [Anthropic](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)
- Harbor (the TB 2 harness) is the official runner, and NVIDIA's NeMo Gym docs describe holding the model fixed while varying only the harness. — [tbench docs](https://tbench.ai/docs); [NeMo Gym](https://docs.nvidia.com/nemo/gym/evaluation/harness)
- Hugging Face's leaderboard example shows a harness quirk (killing background processes) shifting ranks, which a per-benchmark breakdown revealed. — [The New Stack](https://thenewstack.io/deepseek-harness-cleanup-benchmark/)

### Inferences
- Aider's pattern, one fixed cheap benchmark rerun for every harness change, is the model for ocode: pick a frozen suite, pin model, quantization, temperature and seed, change one harness variable, and compare paired.
- The Aider JSON finding is directly relevant to constrained decoding: format constraints can reduce code quality even when they reduce syntax errors. This warrants a paired test on code-quality tasks, not just tool-call validity.

### Gaps
- No sources found on how OpenHands, Claude Code, Codex, Cline or opencode evaluate harness changes internally.
- Aider's current methodology post and leaderboard change logs were not opened.

## 5. Benchmark validity concerns

### Takeaway
Validity failures come in four kinds: leaked solutions (contamination), tests that are too narrow or too wide, specs that do not match tests, and hackable graders. Each major public benchmark has a documented problem, so a custom, privately held suite is a defensible primary signal.

### Cited Findings
- Narrow and wide tests, contamination and gold-patch reproduction in SWE-bench Verified: see section 1. — [OpenAI](https://openai.com/index/why-we-no-longer-evaluate-swe-bench-verified/)
- SWE-bench Pro: about 30% of public tasks broken per OpenAI; 83 of 100 sampled tasks had issues per a less rigorous independent audit. — [OpenAI](https://openai.com/blog/separating-signal-from-noise-coding-evaluations); [LessWrong](https://www.lesswrong.com/posts/nAMhbz5sfpcynjPP5/swe-bench-pro-is-even-worse)
- Terminal-Bench: hackable graders (16-22% of tasks across terminal benchmarks) and task fixes between 2.0 and 2.1. — [arXiv 2610.15924](https://arxiv.org/pdf/2610.15924); [arXiv 2606.08960](https://arxiv.org/html/2606.08960)
- OpenAI's recommended mitigations: password-protect public benchmark data, canary strings in training data filters, extra contamination tests, and privately authored benchmarks. — [OpenAI](https://openai.com/index/why-we-no-longer-evaluate-swe-bench-verified/)
- Epoch AI review series rates Verified, Pro and BFCL v4 "Flawed". — [Epoch](https://epoch.ai/benchmarks/swe-bench-verified/review)
- Infrastructure correlates failures: shared CPU or memory exhaustion makes trials non-independent. — [Anthropic](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)

### Inferences
- A custom suite must be validated with the same checks used on these benchmarks: reference solution passes, base fails, a mutated wrong solution fails, and an agent cannot read or edit tests. Failures of tasks at 0% across trials should be audited as likely grader bugs.
- Canary strings and keeping the suite out of public repos protect against future contamination of ocode's own fixtures.

### Gaps
- No quantification of how contamination affects small open-weight models specifically.
- Epoch review dates and ratings were taken from search summaries and not opened.
