# Harness architecture lessons for ocode (Go, Bubble Tea, Ollama)

Method note: ~18 tool calls. Several sources are search-tool summaries or third-party mirrors; those are flagged. Not covered in depth (no primary source fetched): Aider internals beyond repo map, SWE-agent ACI paper (2024), Cline/Roo/Gemini CLI primary docs, Goose primary docs.

## 1. How major harnesses separate core loop from UI

### Takeaway
The converging pattern is a UI-agnostic core loop emitting typed events, wrapped by a thin transport (JSON-RPC over stdio, HTTP+SSE/WebSocket, or in-process callbacks), with TUI, IDE, headless and eval as peer clients. Claude Code, Codex, OpenHands V1, opencode, Goose and Gemini CLI (reportedly) all do some version of this; ocode already has the seed (`internal/agent`) but the TUI bypasses it.

### Cited Findings
- Claude Code: all surfaces (interactive CLI, headless `claude -p`, Agent SDK, IDE) feed the same `queryLoop()` async generator; only rendering/interaction varies; `QueryEngine` wraps it for non-interactive surfaces. Loop is a small part: "Most of the code, however, lives in the systems around this loop" (community estimate ~1.6% decision logic, 98.4% infrastructure). Source-code study of v2.1.88 — [Architecture of Claude Code, arXiv 2604.14228](https://arxiv.org/html/2604.14228v1)
- Anthropic: the Agent SDK "runs the same execution loop as Claude Code"; loop is gather context -> act -> verify -> repeat, limited by turns/cost/thinking depth — [Agent SDK agent loop docs](https://code.claude.com/docs/en/agent-sdk/agent-loop), [Anthropic engineering post](https://anthropic.com/engineering/building-agents-with-the-claude-agent-sdk) (snippets only)
- Codex: the app-server "powers Codex's rich clients" (VS Code extension is reference client; the TUI can connect to a remote server with `--remote`). JSON-RPC 2.0 over stdio (default), experimental WebSocket/unix socket. Primitives: Thread (conversation) > Turn (one request + work) > Item (message, command, file change, tool call). Flow: initialize, thread/start|resume|fork, turn/start, stream `item/started`/deltas/`item/completed` until `turn/completed`; turns can be steered or interrupted; server-initiated requests ask the client for approvals/elicitation. Experimental APIs gated by capability; bounded queues with overload error -32001. For CI/automation OpenAI recommends the SDK / exec instead — [Codex App Server docs](https://learn.chatgpt.com/docs/app-server) (redirect from developers.openai.com/codex/app-server); schema generation per installed version via `generate-ts`/`generate-json-schema` per [search summary of same docs](https://developers.openai.com/codex/app-server)
- OpenHands V1 SDK: four packages — `openhands.sdk` (Agent, Conversation, LLM, Tool, events), `.tools`, `.workspace` (local/Docker/remote), `.agent_server` (REST + WebSocket). Agent is a stateless immutable spec emitting events via `on_event` callback; Conversation is a factory returning `LocalConversation` (in-process) or `RemoteConversation` (talks to server) — "local-first, deploy-anywhere". CLI, web UI and GitHub App use the core as a shared library — [OpenHands SDK paper, arXiv 2511.03690v2 (MLSys 2026)](https://arxiv.org/html/2511.03690v2)
- opencode: on startup launches an HTTP server and the TUI is a client; `opencode serve` exposes headless OpenAPI server; supports multiple clients (desktop, web, mobile via HTTP+SSE, per third-party write-up) — [opencode server docs](https://opencode.ai/docs/server/); [TheNewStack on opencode 2](https://thenewstack.io/opencode-2-shared-service/) (403 on fetch; snippet only: plumbing for client/server "has been there from the start"). Architecture-map site is third-party.
- Goose (Rust): crates `goose` (core/agent loop), `goose-server` (`goosed`, REST+WebSocket for desktop), `goose-mcp` (builtin extensions), `goose-acp` (ACP server over stdio; reuses same extension registry, permission manager and tool execution). Roadmap discussion suggests consolidating onto ACP and removing goosed/old CLI. Sources are mostly third-party mirrors; verify — [Goose architecture (mintlify mirror)](https://www.mintlify.com/block/goose/development/architecture), [Goose ACP](https://mintlify.com/block/goose/advanced/acp-protocol), [official docs](https://goose-docs.ai/docs/goose-architecture/)
- Gemini CLI: `packages/cli` (input/history/display) vs `packages/core` (API orchestration, tool execution) — only from an unverified mirrored doc — [HF mirror of architecture.md](https://huggingface.co/datasets/LevelUp2x/adaptai/blob/main/projects/gemini-cli/docs/architecture.md)
- Cline: layered SDK (contracts/schemas, provider model layer, stateless agent loop, stateful core owning sessions/checkpoints) — secondary commentary only, unverified — see [Cline docs](https://docs.cline.bot/core-workflows/plan-and-act)
- mini-SWE-agent: no UI; ~100-line agent class + ~100 lines env/model/script — [mini-swe-agent docs](https://mini-swe-agent.com/latest/)

### Inferences
- Common shape for ocode: `agent.Session` (owns messages, tool executor, approvals, mode) -> `<-chan Event`; clients (TUI, `-p` stream-json, eval, future JSON-RPC/ACP server) only consume events and send commands (submit, interrupt, approve, set-mode). Codex's thread/turn/item and OpenHands' Conversation/Event are good naming models.
- Approvals should be a request/response pair on the event stream (Codex server-initiated requests; OpenHands WAITING_FOR_CONFIRMATION state), not a TUI modal calling back into logic. This is what lets headless and eval use a policy instead of a human.
- Feature sprawl (routing, RAG, verification gate, dream mode, research, checkpoints) should move behind core interfaces or hooks; the 98.4%/1.6% Claude Code figure suggests the "infrastructure around the loop" is legitimately big, but it lives in core packages, not the view.

### Gaps
- No primary-source confirmation of Gemini CLI, Cline/Roo, or Goose current internals; Aider's architecture (Coder classes, edit formats) not researched beyond repo map.

## 2. Design decisions authors credit

### Takeaway
Simplicity of the loop is the most consistent credited decision; capability growth made elaborate tooling unnecessary (mini-SWE-agent) while safety and context management consume the engineering effort (Claude Code).

### Cited Findings
- mini-SWE-agent motto: "What if our agent was 100x simpler, and still worked nearly as well?" Authors note SWE-agent (2024) leaned on custom tools/interfaces that a year later were largely unnecessary. Choices: bash only, no model tool-calling interface (works with any model), linear message history (trajectory == model input; helps debugging/fine-tuning), each action a fresh `subprocess.run` (swap `docker exec` for sandboxing). Scores >74% SWE-bench Verified (project claim; Gemini 3 Pro 74% in news item) — [mini-swe-agent docs](https://mini-swe-agent.com/latest/), [PyPI/README summary](https://github.com/SWE-agent/mini-swe-agent). Self-reported; see bash-only leaderboard.
- Claude Code: master loop is single-threaded with one flat message history; planning via TodoWrite; subagents isolated; everything else (compaction, hooks, permissions) is infrastructure. Mostly reverse-engineering blogs, unofficial — [PromptLayer](https://blog.promptlayer.com/claude-code-behind-the-scenes-of-the-master-agent-loop/), [ZenML summary](https://www.zenml.io/llmops-database/claude-code-agent-architecture-single-threaded-master-loop-for-autonomous-coding)
- Claude Code permissions: deny > ask > allow; 7 modes (plan, default, acceptEdits, dontAsk, bypassPermissions, auto, internal bubble); layered defenses incl. classifier and shell sandbox; motivation: users approve ~93% of prompts so human vigilance is unreliable. 27 hook event types (5 permission-related); a PreToolUse allow cannot bypass deny rules. Known weakness: commands with >50 subcommands fall back to a generic prompt — [arXiv 2604.14228](https://arxiv.org/html/2604.14228v1)
- Claude Code context handling: five shapers before each call (budget reduction, snip, microcompact, context collapse, auto-compact), lazy CLAUDE.md loading, deferred tool schemas, summary-only subagent returns — [arXiv 2604.14228](https://arxiv.org/html/2604.14228v1)
- Claude Code subagents: re-enter the same loop with isolated context, return summary, transcript in separate sidechain file — [arXiv 2604.14228](https://arxiv.org/html/2604.14228v1)
- OpenHands V1: immutable/stateless components with a single mutable ConversationState; delegation is just a tool (subagents = independent conversations); SecurityAnalyzer (low/med/high/unknown) + ConfirmationPolicy changeable mid-session; Condenser summarizes history (up to 2x API cost reduction citing prior work). Production rollout: system-attributable errors 78.0 -> 30.0 per 1k conversations (-61%) vs V0 over 15 days; SWE-bench Verified best 76.6% — [arXiv 2511.03690v2](https://arxiv.org/html/2511.03690v2)
- Plan/Act: Cline Plan mode explores without touching files, Act executes, context retained, separate model per mode permitted — [Cline docs](https://docs.cline.bot/core-workflows/plan-and-act). Roo Custom Modes (restricted tools per persona) per secondary guide — [fast.io](https://fast.io/resources/cline-custom-modes/). Gemini CLI plan mode (Mar 2026): read-only tool subset — [Google Developers blog](https://developers.googleblog.com/en/plan-mode-now-available-in-gemini-cli/)
- Aider repo map (Oct 2023, older): tree-sitter definitions, graph ranking, default 1k token budget; model requests files it needs — not embeddings — [Aider blog](https://aider.chat/2023/10/22/repomap.html)

### Inferences
- ocode's explore/plan/write modes match the industry pattern; implement them as tool-allowlist + permission-mode in core config rather than UI state. Per-mode model routing is supported by Cline precedent.
- For small local models the mini-SWE-agent result is less transferable: its score depends on frontier models. Keep this caveat; simplicity of loop still applies, but tool design/ACI may matter more for weak models (SWE-agent's 2024 thesis; not re-verified here).
- A bash-only baseline mode in ocode's eval harness would measure how much the custom tools actually add per local model.

### Gaps
- No author interview found crediting hooks or skills specifically; no ablation data for TodoWrite. Official Anthropic post fetched only as snippets.

## 3. Event-sourced / session-log architectures

### Takeaway
Append-only event logs give resume, fork, audit and eval replay cheaply; OpenHands measured negligible overhead. Claude Code and Codex use the same idea (JSONL transcripts; thread resume/fork).

### Cited Findings
- OpenHands V1: all interactions immutable events in an append-only log; each event its own JSON file plus `base_state.json`; resume loads state and replays; agents detect incomplete conversations and continue. Measured on 433 SWE-bench conversations: median persist 0.20 ms/event, full replay 4.1 ms, crash recovery 7.4 ms (paper text and table disagree slightly on max-case recovery: "<20 ms" vs 32.1 ms) — [arXiv 2511.03690v2](https://arxiv.org/html/2511.03690v2)
- Claude Code: append-only JSONL transcripts; resume/fork rebuild from them; session-scoped permissions deliberately not restored on resume; subagent sidechain files — [arXiv 2604.14228](https://arxiv.org/html/2604.14228v1)
- Codex app-server: thread fork/archive/list/resume as first-class methods — [Codex App Server docs](https://learn.chatgpt.com/docs/app-server)
- mini-SWE-agent: linear history means trajectory == LLM input, simplifying debug/fine-tune — [docs](https://mini-swe-agent.com/latest/)

### Inferences
- ocode: make a JSONL event log the single source of truth (messages, tool calls/results, approvals, mode changes, checkpoints, condensation markers). TUI state, `-p` stream-json, eval replays and LoRA export all derive from it; this would delete much per-feature state from the Bubble Tea Model. Distinguish LLM-visible events from internal ones (OpenHands `to_llm_message`).
- Replaying tool results deterministically enables regression evals without re-running the model.

### Gaps
- Did not verify Codex's rollout file format or opencode's storage format.

## 4. ACP and editor protocols

### Takeaway
ACP has real multi-vendor traction (Zed, JetBrains, Neovim, Emacs; Gemini CLI, Claude Code adapter, Codex, Goose). If ocode gets a clean event core, an ACP stdio server is a modest adapter and buys editor reach; do not adopt it before the core split.

### Cited Findings
- ACP introduced Aug 2025 by Zed with Google (Gemini CLI reference implementation); Claude Code and Codex available in Zed via ACP (Oct 2025 for Codex); JetBrains co-develops; Neovim (CodeCompanion) and Emacs (agent-shell) support; ACP Registry launched 28 Jan 2026; Zed shifting from building adapters to protocol stewardship; Apache licensed — [Zed ACP page](https://zed.dev/acp), [ACP progress report](https://zed.dev/blog/acp-progress-report), [ACP Registry](https://zed.dev/blog/acp-registry), [JetBrains on ACP](https://zed.dev/blog/jetbrains-on-acp). Mostly vendor/partner sources; no independent usage metrics found.
- Goose `GooseAcpAgent` reuses its normal extension registry, permission manager, tool execution — [Goose ACP](https://mintlify.com/block/goose/advanced/acp-protocol) (third-party mirror)
- Codex app-server is OpenAI's own protocol for rich clients (not ACP), with generated TS/JSON schemas — [Codex docs](https://learn.chatgpt.com/docs/app-server)

### Inferences
- Both ACP and Codex app-server are JSON-RPC over stdio with session/turn/streamed-update/permission-request primitives; designing ocode's internal event types to map onto these makes either adapter thin. ocode's `-p` stream-json is already a one-way subset.
- Worth it: an `ocode acp` subcommand is low-cost editor distribution for a local-model agent (Zed/JetBrains/Neovim users) and forces the clean core boundary.

### Gaps
- Did not read the ACP spec itself (method list, capability negotiation, Go SDK availability). Check for an official Go library before building.

## 5. Do multi-agent and RAG help or hurt?

### Takeaway
Evidence favors single-threaded writes, read-only/clean-context helpers, and agentic grep/glob search over indexed RAG for coding agents, with caveats (informal evidence; semantic search wins on very large repos; local models differ).

### Cited Findings
- Cognition "Don't Build Multi-Agents" (Jun 2025, Walden Yan): context sharing and implicit decisions in actions break parallel agents — [Cognition](https://cognition.ai/blog/dont-build-multi-agents)
- Cognition follow-up (Apr 2026): "multi-agent systems work best today when writes stay single-threaded and the additional agents contribute intelligence rather than actions." Works: clean-context review (Devin Review ~2 bugs/PR, ~58% severe; best when reviewer shares no prior context), strong-model "smart friend" escalation, manager/child map-reduce. Does not work: parallel-writer swarms, weak primary + strong advisor, naive delegation — [Multi-agents: what's actually working](https://cognition.com/blog/multi-agents-working)
- Claude Code's Boris Cherny (Latent Space, May 2025): early versions used RAG with Voyage embeddings; settled on agentic search (glob/grep) citing performance and avoiding indexing/security downsides; "at the cost of latency and tokens"; evidence "mostly vibes" plus some internal benchmarks — summarized in [OfficeChai](https://officechai.com/ai/claude-researcher-explains-how-agentic-search-performed-better-than-rag-for-code-generation/) (secondary; original podcast not fetched)
- Counter-evidence: turbopuffer talk reports semantic search gave 2.6% higher code retention on repos >=1,000 files — [AI Engineer talk](https://ai.engineer/talks/UM6sFg_jdlE-rag-is-dead-right-kuba-rogut) (snippet only)
- Claude Code subagents are read/research style with summary-only return; OpenHands delegation is blocking-parallel independent conversations — [arXiv 2604.14228](https://arxiv.org/html/2604.14228v1), [arXiv 2511.03690v2](https://arxiv.org/html/2511.03690v2)
- Aider chose a ranked structural repo map, not embeddings (2023) — [Aider](https://aider.chat/2023/10/22/repomap.html)

### Inferences
- Candidates for ocode demotion/removal: `parallel_edit` (parallel writers: the pattern Cognition says fails), RAG as default path (replace with grep/glob plus an optional Aider-style repo map, which also suits small context windows), model-routing complexity unless eval-proven. Keep read-only explore subagents and a fresh-context verification/review pass (matches ocode's verification gate).
- Local models have small context and weaker tool-use, so the agentic-search token/latency cost bites harder than for Claude; a cheap repo map is a plausible middle path. This needs ocode's own eval to decide; no published local-model evidence found.
- Peripheral features (voice, LoRA export, dream mode, guided web research) have no comparable analogue in the leading harnesses surveyed; treat as plugins/separate binaries behind the event protocol or hooks/MCP, not core.

### Gaps
- No controlled ablation of RAG vs agentic search for small local models; no source on dream-mode-like idle agents.

## 6. Concrete lessons for ocode (synthesis)
1. Define `agent.Event` + `agent.Command` types and one `Session` API; make TUI, `-p`, eval consume only that (OpenHands on_event, Codex thread/turn/item).
2. Move approvals, mode, routing, verification gate, checkpoints into core as policies/middleware; TUI renders and answers requests.
3. Append-only JSONL session log as source of truth (resume, fork, replay, eval, export).
4. Optional thin servers later: stdio JSON-RPC / ACP adapter first (cheapest), HTTP+SSE like opencode if remote/multi-client is wanted.
5. Prune per evidence: parallel writers, default RAG; make the rest opt-in extensions via hooks/MCP.
6. Keep a minimal-loop baseline (mini-SWE-agent style) in evals to measure whether each feature earns its place per local model.
