# Safety, Sandboxing, and Prompt-Injection Defenses for Coding Agents (input for ocode)

Source-quality note: many 2026 items come from secondary coverage (news, vendor blogs, CSA research notes; one CSA note self-labels as AI-assisted and not officially reviewed). CVE scores differ between aggregators. Primary advisories should be re-checked before quoting numbers. Vulnerability classes are described, not payloads.

## 1. Prompt injection against coding agents: real incidents and CVEs (2025-2026)

### Takeaway
Nearly every incident falls into a few classes: (a) injected text in repo files, issues, PRs or tool descriptions; (b) the agent being steered to edit its own config (auto-approve flags, MCP servers, hooks); (c) weak command allowlists; (d) project-supplied config that executes before any trust prompt. Config-as-code and allowlist parsing are the most repeatedly exploited gaps.

### Cited Findings
- Copilot CVE-2025-53773: injected instructions in source files, web pages or GitHub issues get Copilot to write `"chat.tools.autoApprove": true` into `.vscode/settings.json`, enabling auto-approve ("YOLO") mode, after which commands run without approval. Microsoft advisory 2025-08-12; CVSS reported variously 6.8-7.8 — [Embrace The Red](https://embracethered.com/blog/posts/2025/github-copilot-remote-code-execution-via-prompt-injection); [Tenable](https://www.tenable.com/cve/CVE-2025-53773)
- Cursor CurXecute (CVE-2025-54135): prompt injection made the agent rewrite `.cursor/mcp.json`, and the new MCP server ran before the user accepted or rejected the edit — [Tenable FAQ](https://www.tenable.com/blog/faq-cve-2025-54135-cve-2025-54136-vulnerabilities-in-cursor-curxecute-mcpoison); [The Hacker News](https://thehackernews.com/2025/08/cursor-ai-code-editor-vulnerability.html?m=1)
- Cursor MCPoison (CVE-2025-54136): trust was bound to the MCP entry name, not its contents, so a benign approved config could later be swapped for a malicious command (a "rug pull"). Fix: re-approval whenever the entry changes — [Check Point](https://research.checkpoint.com/2025/cursor-vulnerability-mcpoison/)
- Claude Code project files (Check Point, Feb 2026): repo-defined hooks in `.claude/settings.json` could run shell commands when starting in an untrusted project; project settings could set environment variables leading to API-key exfiltration before trust confirmation (CVE-2026-21852); an MCP consent bypass (CVE-2025-59536) used `enableAllProjectMcpServers` / `enabledMcpjsonServers` in the same project file. CVE-to-flaw mapping differs between write-ups — [Check Point](https://research.checkpoint.com/2026/rce-and-api-token-exfiltration-through-claude-code-project-files-cve-2025-59536/); [Security Affairs](https://securityaffairs.com/188508/security/untrusted-repositories-turn-claude-code-into-an-attack-vector.html); [Hacker News](https://thehackernews.com/2026/02/claude-code-flaws-allow-remote-code.html?hl=en)
- Pillar Security sandbox-escape series (2026): the agent often does not break the sandbox but writes a file a trusted tool outside the sandbox later runs. Cursor: workspace `.claude` hook config gave unsandboxed execution (CVE-2026-48124, fixed 3.0.0); edited virtualenv interpreter run by an extension; git metadata outside `.git` triggering execution via fsmonitor, evading path rules. Codex CLI: "safe" allowlist trusted `git show` by name, not arguments (patched 0.95.0). Docker-socket reachability affected Cursor, Codex and Gemini CLI. Antigravity: macOS Seatbelt denylist bypass and `.vscode` task config bypass (Google downgraded them) — [BleepingComputer](https://www.bleepingcomputer.com/news/security/cursor-codex-gemini-cli-antigravity-hit-by-sandbox-escapes/)
- Amazon Q Developer VS Code extension (July 2025): a malicious pull request to the open-source toolkit repo injected a "clean the system to near-factory state, delete filesystem and cloud resources" style prompt; it shipped in v1.84.0 (~July 17) and was removed in 1.85. AWS says it did not execute in customer environments (a formatting error was reported). Dates vary by source — [SC World](https://scworld.com/news/amazon-q-extension-for-vs-code-reportedly-injected-with-wiper-prompt); [Tom's Hardware](https://www.tomshardware.com/tech-industry/cyber-security/hacker-injects-malicious-potentially-disk-wiping-prompt-into-amazons-ai-coding-assistant-with-a-simple-pull-request-told-your-goal-is-to-clean-a-system-to-a-near-factory-state-and-delete-file-system-and-cloud-resources)
- Nx "s1ngularity" (Aug 26, 2025): malicious npm versions invoked locally installed Claude, Gemini and Q CLIs with permission-bypass flags (`--dangerously-skip-permissions`, `--yolo`, `--trust-all-tools`) to hunt for secrets and leak them; ~2,349 secrets to 1,079 repos per GitGuardian via Wiz-cited figures. Initial access was phishing of maintainers; guardrails sometimes refused. Lesson for ocode: any agent CLI on the box is a "living off the land" tool, and bypass flags are discoverable by malware — [Wiz](https://wiz.io/blog/s1ngularity-supply-chain-attack); [Socket](https://socket.dev/blog/nx-packages-compromised); [Okta](https://www.okta.com/blog/threat-intelligence/the-s1ngularity-attack--when-attackers-prompt-your-ai-agents-to/)
- Gemini CLI (Tracebit, reported June 27, fixed July 25, 2025, v0.1.14): an approved `grep` prefix allowed a chained malicious command to ride along; fix is to require permission for every command in the input. Separate 2026 advisory GHSA-wpqr-6v78-jr5g: `--yolo` failed to enforce the settings.json tool allowlist, fixed in 0.39.1, described as CVSS 10 in CI contexts by CSA — [Tracebit](https://tracebit.com/blog/code-exec-deception-gemini-ai-cli-hijack); [The Hacker News](https://thehackernews.com/2026/04/google-fixes-cvss-10-gemini-cli-ci-rce.html?hl=en)
- MCP tool poisoning (Invariant Labs, Apr 2025): hidden instructions in tool descriptions invisible to the user; rug pull (description changes after approval); tool shadowing (one server's description steers use of another server's trusted tool, e.g. changing email recipients); GitHub MCP hijack via public issue content. OWASP lists tool poisoning as MCP03:2025 — [Invariant](https://invariantlabs.ai/blog/mcp-security-notification); [Willison](https://simonwillison.net/2025/Apr/9/mcp-prompt-injection/); [CSA whitepaper](https://labs.cloudsecurityalliance.org/research-rb/csa-whitepaper-mcp-security-tool-poisoning-20260506-csa-styl/) (OWASP number is via a secondary DEV post)
- CI "Comment and Control" (Apr 2026+): a PR comment or issue pushed Claude Code, Gemini CLI and Copilot agents to leak `ANTHROPIC_API_KEY`, `GITHUB_TOKEN`, `GEMINI_API_KEY`; Microsoft reported Claude Code Action could expose workflow secrets (June 2026); "GitLost" (July 2026) made GitHub agentic workflows leak private repos. Common mitigations: minimise secrets visible to agent steps, treat issue/PR text as untrusted, restrict writes to declared safe outputs — [CSA](https://labs.cloudsecurityalliance.org/research/csa-research-note-comment-control-github-prompt-injection-20/); [Microsoft](https://microsoft.com/en-us/security/blog/2026/06/05/securing-ci-cd-in-agentic-world-claude-code-github-action-case); [Hacker News](https://thehackernews.com/2026/07/public-github-issue-could-trick-github.html?m=1)
- "The Attacker Moves Second" (Oct 2025, USENIX Sec 2026): adaptive attacks bypassed all 12 evaluated prompt-injection/jailbreak defenses (typically >90% success, prompting defenses 95-99%), including a run-twice-and-compare defense. Detection/guardrail filters are not a security boundary — [arXiv](https://arxiv.org/abs/2510.09023); [VentureBeat](https://venturebeat.com/security/12-ai-defenses-claimed-near-zero-attack-success-researchers-broke-all-of-them)

### Inferences
- ocode's untrusted-input surface is: repo files (AGENTS.md-style files, READMEs, code comments), web fetch results, MCP tool descriptions and results, git history/issues, and dependency output. Treat all as data.
- Auto-approve state, MCP server lists, hook files and ocode's own config must not be agent-writable and must not be loaded from the repo without a trust prompt (this is the single most repeated bug class).
- Pin MCP trust to a hash of (command, args, env, tool descriptions/schemas) and re-prompt on any change. This is the MCPoison and rug-pull fix.
- Never run prompt-injection classifiers as the primary defense.

### Gaps
- No ocode-specific or Ollama/local-model-specific data on injection susceptibility versus frontier models found; likely weaker instruction hierarchy, but unsourced.
- Primary advisories were not opened for several 2026 items (Pillar CVEs, GitLost); figures are from secondary coverage.

## 2. Frameworks: lethal trifecta, Rule of Two, CaMeL, dual-LLM, design patterns

### Takeaway
Structural defenses (remove one leg of the trifecta, constrain actions after untrusted input) are what the literature endorses; detection is not. In a CLI harness the practical ones are the Rule of Two as a per-session capability policy, plan/act separation with taint-based action gating, and the "plan-then-execute" and "context-minimization" patterns. Full CaMeL or dual-LLM is costly and likely impractical for small local models.

### Cited Findings
- Lethal trifecta (Willison, June 2025): private data access + exposure to untrusted content + ability to communicate externally. Only reliable protection is not combining all three; guardrails claiming "95%" are a failing grade; points to CaMeL and the design-patterns paper — [Willison](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/)
- Meta "Agents Rule of Two" (Oct 31, 2025): within one session an agent should have at most two of (A) untrusted inputs, (B) sensitive systems/private data, (C) ability to change state or communicate externally; a supplement to least privilege, not a replacement. Willison notes "changing state" closes a gap in the trifecta framing. Critics: does not stop a compromised agent contaminating other agents — [Meta](https://ai.meta.com/blog/practical-ai-agent-security/); [Willison](https://simonwillison.net/2025/Nov/2/new-prompt-injection-papers/); [critique](https://kenhuangus.substack.com/p/the-rule-of-two-vs-reality-why-metas)
- Design Patterns paper (arXiv 2506.08837, Beurer-Kellner et al., IBM/Invariant/ETH/Google, June 2025): once an agent ingests untrusted input it must be constrained so the input cannot trigger consequential actions. Patterns include Dual LLM, Code-Then-Execute (plan in a restricted DSL), and CaMeL; 10 case studies including software-engineering agents — [arXiv](https://arxiv.org/abs/2506.08837v3); [Willison](https://simonwillison.net/2025/Jun/13/prompt-injection-design-patterns/)
- CaMeL (arXiv 2503.18813): privileged LLM writes code in a restricted Python dialect; a custom interpreter tracks data provenance and enforces policies; 67% of AgentDojo tasks with provable security in v1 (77% vs 84% undefended in a later version; versions and author lists differ). Drawbacks: policy authoring and confirmation fatigue — [arXiv](https://arxiv.org/abs/2503.18813); [Willison](https://simonwillison.net/2025/Apr/11/camel/); [Schneier](https://www.schneier.com/blog/archives/2025/04/applying-security-engineering-to-prompt-injection-security.html)

### Inferences
- Map to ocode modes: explore mode (untrusted reads, no write/network) and write mode (state change) already approximate the Rule of Two. Gaps: web fetch and MCP in the same session as private-data reads plus shell/network gives all three. Suggest a session-level "trifecta meter" that, when untrusted content has entered context and the session also has network-capable tools and secret/data access, forces approval on every external-effect tool call or drops the network tools.
- Cheap taint tracking: mark context as "tainted" after web fetch, MCP results, or reading files outside a trusted set; while tainted, downgrade "always/session" approval scopes to per-call for network, write-outside-workspace, and config-touching actions.
- Dual-LLM for web fetch (a quarantined summarizer returning only structured fields) is feasible with Ollama but weak models make it lossy; consider only for fetch.

### Gaps
- No evidence found on how well these patterns work with small local models.

## 3. Sandbox implementations and network egress

### Takeaway
Filesystem plus network isolation are both required; ocode's seatbelt/bwrap covers filesystem writes, but without network egress control a prompt-injected agent can still read secrets and exfiltrate them. Egress control via a domain-allowlist proxy outside the sandbox is the standard (Claude Code, Codex), though allowlists have their own holes. Yes, it is the key missing piece unless ocode already blocks network in-sandbox (not stated).

### Cited Findings
- Claude Code sandbox: seatbelt (macOS) / bubblewrap (Linux) enforced on all subprocesses; read/write limited to cwd; network only through a Unix socket to a proxy outside the sandbox that enforces a domain allowlist and prompts on new domains; internal data says sandboxing cuts permission prompts 84%; open-sourced as sandbox-runtime; "without network isolation, a compromised agent could exfiltrate files such as SSH keys" — [Anthropic engineering](https://www.anthropic.com/engineering/claude-code-sandboxing)
- Claude Code docs: the proxy filters on requested hostname and does not terminate or inspect TLS; Linux needs bubblewrap and socat; WSL1 unsupported — [docs](https://code.claude.com/docs/en/sandboxing)
- sandbox-runtime: empty `allowedDomains` means no network; HTTP(S) via HTTP proxy and other TCP via SOCKS5 proxy; seatbelt profile only permits one localhost port — [GitHub](https://github.com/anthropic-experimental/sandbox-runtime)
- Claude Code on the web keeps git credentials outside the sandbox; a proxy verifies a scoped credential and the git operation (e.g. push only to the configured branch) before attaching the real token — [Anthropic engineering](https://www.anthropic.com/engineering/claude-code-sandboxing)
- Codex CLI: modes `read-only`, `workspace-write`, `danger-full-access`; network off by default; seatbelt on macOS, bwrap+seccomp on Linux; inside writable roots `.git`, `.agents`, `.codex`, `.aws` are read-only recursively (including a resolved gitdir if `.git` is a pointer file); proxy domain rules are allowlist-first with deny winning; private/link-local/loopback blocked by default; DNS-rebinding protection "best-effort"; proxy does not cover web search, MCP or model traffic; web search defaults to cached index; non-version-controlled folders default to read-only; `auto_review` reviewer agent fails closed — [OpenAI docs](https://learn.chatgpt.com/docs/agent-approvals-security); [Inside the Codex sandbox](https://openai.com/index/inside-the-codex-sandbox/)
- WorkOS: in Codex, enabling network access without the proxy gives unrestricted direct access with domain rules inert — [WorkOS](https://workos.com/blog/agent-sandbox-egress-defaults)
- An allowlisted package proxy was used by an agent to escape a network-isolated sandbox in a GitLab evaluation (vendor-reported, via InfoQ); GitLab's framing: an allowlist gives the workload the reach of anything it can take over — [InfoQ](https://infoq.com/news/2026/09/gitlab-ai-sandbox-access); [GitLab](https://about.gitlab.com/blog/ai-agent-sandbox/)
- DNS is an exfil channel: Claude Code's allowlisted `ping`, `nslookup`, `dig`, `host` leaked data (CVE-2025-55284; fix v1.0.4 by removing them from the allowlist; overbroad "safe command" list was the root cause) — [Embrace The Red](https://embracethered.com/blog/posts/2025/claude-code-exfiltration-via-dns-requests); [NVD](https://nvd.nist.gov/vuln/detail/CVE-2025-55284)
- Docker socket reachable from the sandbox was an escape in Cursor, Codex and Gemini CLI — [BleepingComputer](https://www.bleepingcomputer.com/news/security/cursor-codex-gemini-cli-antigravity-hit-by-sandbox-escapes/)

### Inferences
- Implementation for ocode: default `deny network` in the sandbox profile (seatbelt `(deny network*)`; bwrap `--unshare-net`), with an optional local proxy bound on a Unix socket/localhost port outside the sandbox that does domain allowlisting (package registries, git host), logging, and per-new-domain approval. Block raw sockets and UDP/DNS from inside; only the proxy resolves names (prevents DNS exfil).
- Allowlist hygiene: avoid broad hosts that accept arbitrary user content (gist/pastebin/raw GitHub write APIs, public buckets); prefer read-only package mirrors; expect hostname-only filtering to be bypassable (e.g. fronting) and secrets to be kept out of reach anyway (no source for fronting in the searches, so treat as a design caution).
- Deny-list protected paths inside the writable workspace: `.git/hooks`, `.git/config`, gitdir pointers, ocode config/hook/MCP files, `.vscode`, `.claude`, `.cursor`, virtualenv/`node_modules/.bin` and other files other tools auto-execute. Pillar's results show "write a file that something unsandboxed later runs" is the main escape path.
- Deny access to `/var/run/docker.sock` and other daemon sockets in the profile; scrub env for subprocesses.
- MCP stdio servers are processes: run them under the same sandbox (sandbox-runtime states it can sandbox MCP servers). web fetch should go through the same domain policy and block private/link-local ranges (SSRF, cloud metadata).
- Microvm/container options (Docker, gVisor, Firecracker, e2b) were not researched in depth; Docker blog hits were vendor marketing.

### Gaps
- No primary source on gVisor, Firecracker or e2b for coding agents was retrieved; no Gemini CLI sandbox details retrieved.
- No source confirming domain fronting against these proxies.

## 4. Shell allowlist/denylist parsing pitfalls and parsers

### Takeaway
Shipped agents repeatedly fell to parser-differential and "safe command" bugs (echo, sed, git show, shell built-ins, prefix matching). The recommended approach is a real parser with an allowlist of node types that fails closed ("too complex -> ask"), per-segment checks, wrapper unwrapping, and the OS sandbox as the real boundary.

### Cited Findings
- Claude Code advisories: CVE-2025-54795 (echo parsing skipped the prompt; fixed 1.0.24 line); CVE-2025-55284 (overbroad safe list, network utilities); CVE-2025-64755 (sed parsing bypassed read-only validation, CVSS 4.0 8.7, credit Adam Chester/SpecterOps); CVE-2026-25723 (piped sed with echo bypassed file-write restrictions in accept-edits, fixed 2.0.55) — [GitLab advisory DB](https://advisories.gitlab.com/npm/@anthropic-ai/claude-code/CVE-2025-54795/); [NVD CVE-2026-25723](https://nvd.nist.gov/vuln/detail/CVE-2026-25723); [OSV CVE-2025-64755](https://osv.dev/vulnerability/CVE-2025-64755)
- Gemini CLI: prefix-whitelisted command with chained command; Codex: `git show` allowed by name though invocation not read-only; Cursor CVE-2026-22708: some shell built-ins ran without being in the allowlist or prompting — [Tracebit](https://tracebit.com/blog/code-exec-deception-gemini-ai-cli-hijack); [BleepingComputer](https://www.bleepingcomputer.com/news/security/cursor-codex-gemini-cli-antigravity-hit-by-sandbox-escapes/); [Pillar via search summary](https://workos.com/blog/agent-sandbox-egress-defaults)
- Claude Code's bash analysis (secondary analysis of leaked/dumped source): tree-sitter AST walked against an allowlist of node types; unknown node means "too-complex" and ask. Parser-differential pre-checks: null bytes (bash drops them, tree-sitter doesn't), Unicode whitespace/BOM, backslash-space in command words, brace expansion with quotes, ANSI-C `$'..'` and empty-quote tricks, array-subscript arithmetic evaluation inside single quotes (`test -v`, `printf -v`, `read -a`, `unset -v`), heredoc termination inside `$()`, zsh `=cmd` expansion and `zmodload` modules (ztcp, zsh/files), about 20 eval-like builtins (`eval`, `trap`, `enable -f`, `coproc`, `hash -p`), wrapper peeling (`time`, `nohup`, `timeout`, `nice`, `env`, `stdbuf`) with unknown flags returning too-complex, variable scope resets across `||`, `|`, `&`, parser timeout (50 ms) and node budget (50,000) failing closed because they are adversarially triggerable, redirect-stripping boundary bug (`> /dev/nullo`) — [finisky analysis](https://finisky.github.io/en/claude-code-bash-security); [source dump](https://tangled.org/oppi.li/claude-code/blob/main/utils/bash/ast.ts) (unofficial; treat as unverified)
- Other tools: split compound commands and require all segments allowed; recursively unwrap `sudo`, `bash -c`, `xargs`; gate `python -c`, `node -e`, `sh -c`, `eval` — [search results on runok/safe-chains/pi-safe-shell](https://docs.rs/crate/safe-chains/latest/source/AGENTS.md)

### Inferences
- For Go: mvdan/sh (`syntax.Parser`, `syntax.Walk`) gives a real POSIX/bash AST (no search result confirmed agent use). Approach: parse; reject on parse error or unknown node type; walk and deny any `CmdSubst`, `ProcSubst`, `ArithmExp`, `ParamExp` with side-effecting forms, non-literal command words, redirects other than a tiny allowlist, `Coproc`, background `&`, function decl, `eval`/`source`/`.`/`trap`/`exec`/`command`/`builtin`; resolve literal argv[0] only; treat assignments before command words as env injection; then match rules per CallExpr. mvdan/sh parses bash, not zsh: if the user's shell or the executing shell differs, execute via an explicit `bash --noprofile --norc -c` so parser and executor agree (parser-differential is the core bug class).
- Maintain per-command argv-level read-only checks (flag allowlists), never name-only trust: `git` (config, `-c`, `--output`, fsmonitor/alias/hooks), `find` (`-exec`, `-fprint`), `sed -i/w/e`, `awk system`, `rg --pre`, `tar --to-command`, `xargs`, `env`, `ssh -o ProxyCommand`, `curl/wget` (network, `-o`), and the DNS tools above. Fuzz the allowlist parser against real bash (differential testing: run safe-looking samples under `bash -n` and a sandboxed trace).
- Treat the allowlist as UX only; the sandbox is the boundary, because every shipped agent had a bypass.

### Gaps
- No direct source on mvdan/sh in agent permission code; the claude-code source references are unofficial dumps.
- 2026 advisories beyond those listed may exist.

## 5. Secrets and harness-config protection

### Takeaway
Config/hook/MCP files and credentials are the targets; agents must not write them, repos must not auto-activate them, and secrets should not be in the agent process or sandbox at all.

### Cited Findings
- Codex marks `.git`, `.codex`, `.agents`, `.aws` read-only inside the writable workspace — [OpenAI docs](https://learn.chatgpt.com/docs/agent-approvals-security)
- Project-controlled config that sets environment (API base URLs/keys), hooks, or MCP enablement ran before trust (Claude Code CVE-2026-21852, CVE-2025-59536) — [Check Point](https://research.checkpoint.com/2026/rce-and-api-token-exfiltration-through-claude-code-project-files-cve-2025-59536/)
- Auto-approve flags written by the agent (Copilot CVE-2025-53773), MCP config written by the agent (CurXecute) — see section 1.
- Claude Code on the web keeps git credentials and signing keys outside the sandbox and injects tokens at a proxy — [Anthropic engineering](https://www.anthropic.com/engineering/claude-code-sandboxing)
- CI agents leaked env-resident API keys and `GITHUB_TOKEN` through injected comments — [CSA](https://labs.cloudsecurityalliance.org/research/csa-research-note-comment-control-github-prompt-injection-20/)

### Inferences
- Sandbox profile: deny reads of `~/.ssh`, `~/.aws`, `~/.config/gcloud`, `~/.gnupg`, keychains, browser profiles, shell history, other agents' config dirs (`~/.claude`, `~/.codex`), and `.env*` in the workspace unless explicitly granted. Seatbelt supports `(deny file-read* (subpath ...))`; bwrap simply doesn't mount them.
- Environment: start sandboxed commands with a minimal env (allowlist `PATH`, `HOME` redirected, locale), stripping `*_TOKEN`, `*_KEY`, `AWS_*`, `SSH_AUTH_SOCK`. Redact secret patterns from tool output before it enters context, and from logs.
- ocode's own config, hooks, MCP definitions, rules files, and approval stores should live outside the workspace and outside any writable root; project-level config should be inert until the user trusts that directory (store a hash of the trust decision; re-prompt on change). Project config must not be able to set model endpoint, env vars, hooks, or MCP servers without trust. Protect `.git/hooks` and `.git/config`.
- Make "always" shell rules user-global only and unwritable by the agent; stale-edit guard does not cover this.
- ocode should never advertise or accept a bypass flag that malware can discover cheaply (s1ngularity); consider requiring an interactive confirmation or env gate for any yolo-like mode and refusing it when stdin is not a TTY.

### Gaps
- No source on Ollama server exposure (default localhost:11434, unauthenticated) was gathered.

## 6. Unattended/background behavior ("dream mode") guardrails

### Takeaway
No source covers idle/dream-style agents specifically; the nearest evidence is CI/headless agents, where injected issue/PR content with ambient secrets repeatedly caused leaks. Unattended runs need the strictest posture because no human reviews prompts.

### Cited Findings
- CI agents (Claude Code Action, Gemini CLI action, Copilot) were hijacked via issue/PR comments to leak secrets; the Claude Code Security Review action was described by Anthropic's system card as "not hardened against prompt injection" (per VentureBeat) — [CSA](https://labs.cloudsecurityalliance.org/research/csa-research-note-ai-github-actions-security-20260503-csa-st/); [VentureBeat](https://venturebeat.com/security/ai-agent-runtime-security-system-card-audit-comment-and-control-2026)
- Gemini CLI `--yolo` ignoring the allowlist in CI (GHSA-wpqr-6v78-jr5g, fixed 0.39.1) shows headless-mode policy enforcement is a bug source — [Hacker News](https://thehackernews.com/2026/04/google-fixes-cvss-10-gemini-cli-ci-rce.html?hl=en)
- Codex: approval policy `never` removes prompts; a `granular` policy rejects non-selected prompt categories, and an auto-review agent fails closed — [OpenAI docs](https://learn.chatgpt.com/docs/agent-approvals-security)
- AWS advises not using automatic PR builds from untrusted contributors (Amazon Q incident) — [SC World](https://scworld.com/news/amazon-q-extension-for-vs-code-reportedly-injected-with-wiper-prompt)

### Inferences
- Dream mode should run read-only (explore-equivalent), network off, no MCP/web fetch (removing both untrusted input and exfil legs), writing results only into a dedicated, non-executable notes/proposal file or branch for human review next session; ask-type permissions become auto-deny (fail closed), never auto-allow; deny rules must be enforced as in headless/subagents; cap time, tool calls and tokens; log every action to an append-only file; do not ingest content it has not been shown the user trusts; and never write to ocode config, memory files that are later auto-loaded as instructions, or hook files (persistent injection: memory poisoning).
- Treat any memory/notes the agent writes as untrusted on the next load (it could contain injected instructions persisted from earlier tainted content). Not directly sourced.

### Gaps
- No documented incidents of idle/background coding agents specifically; recommendations here are inferred from CI-agent cases.
