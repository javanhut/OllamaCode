package tools

import (
	"encoding/json"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// ToolMode is a provider-independent mode bitmask. Keeping policy beside the
// registry gives every caller (TUI, subagents, MCP adapters) one source of
// truth without making the tools package depend on the UI package.
type ToolMode uint8

const (
	ModeExplore ToolMode = 1 << iota
	ModePlan
	ModeWrite
	ModeAuto

	ModeReadOnly     = ModeExplore | ModePlan | ModeWrite | ModeAuto
	ModeExploreShell = ModeExplore | ModeWrite | ModeAuto
	ModeMutable      = ModeWrite | ModeAuto
)

type ToolCost string

const (
	ToolCostLow  ToolCost = "low"
	ToolCostHigh ToolCost = "high"
)

// ToolPolicy describes availability and safety properties enforced by the
// harness. It is deliberately not serialized into provider tool definitions.
type ToolPolicy struct {
	Modes          ToolMode
	SmallModelSafe bool
	Destructive    bool
	Network        bool
	Cost           ToolCost
	// Timeout is how long a caller waits before declaring the handler wedged.
	// It rides on the policy because the TUI dispatcher and the headless
	// Executor both need it, and the two name switches that used to answer this
	// question had already drifted apart (one defaulted to 90s, the other to
	// 2m) while a newly registered tool silently got whichever it hit.
	Timeout time.Duration
}

// Deadline classes. These are stuck-handler budgets, not performance targets.
// They are applied by name below rather than folded into the capability groups
// because the two classifications are genuinely orthogonal: get_project_tree is
// a lean read that can walk a huge repo, git_branch is destructive but instant.
const (
	inspectTimeout = 30 * time.Second // local read-only lookup
	mutateTimeout  = 90 * time.Second // local write or git plumbing
	networkTimeout = 2 * time.Minute  // leaves the machine
	longTimeout    = 10 * time.Minute // runs a whole nested agent
	// run_shell kills at its own deadline and then reports; the harness
	// deadline has to outlive that or the report loses the race.
	shellDefaultTimeout = 30 * time.Second
	shellMaxTimeout     = 300 * time.Second
	shellGrace          = 5 * time.Second
)

// DefaultToolTimeout covers anything unclassified, including external/MCP tools.
// A var, not a const, so the watchdog test can shrink it (it is read at call
// time only on the unknown-tool path; the table below bakes its value at init).
var DefaultToolTimeout = 2 * time.Minute

func (p ToolPolicy) Allows(mode ToolMode) bool { return p.Modes&mode != 0 }

func policy(modes ToolMode, small, destructive, network bool, cost ToolCost) ToolPolicy {
	return ToolPolicy{Modes: modes, SmallModelSafe: small, Destructive: destructive, Network: network, Cost: cost}
}

var toolPolicies = func() map[string]ToolPolicy {
	m := map[string]ToolPolicy{}
	read := policy(ModeReadOnly, false, false, false, ToolCostLow)
	leanRead := policy(ModeReadOnly, true, false, false, ToolCostLow)
	mutate := policy(ModeMutable, false, true, false, ToolCostLow)
	leanMutate := policy(ModeMutable, true, true, false, ToolCostLow)

	for _, name := range []string{
		"read_file", "list_directory", "find_files", "grep", "file_info",
		"get_working_directory", "get_project_tree", "git_status", "git_diff",
		"git_log",
	} {
		m[name] = leanRead
	}
	for _, name := range []string{
		"read_session_notes", "remember", "recall", "forget", "find_symbol",
		"code_definition", "code_references", "code_hover", "code_index", "semantic_search",
		"ask_user", "hash_file", "process_list",
		"disk_usage", "spawn_subagent", "job_list", "job_output",
	} {
		m[name] = read
	}
	for _, name := range []string{"web_fetch", "web_search", "web_search_api", "web_crawl"} {
		m[name] = policy(ModeReadOnly, true, false, true, ToolCostHigh)
	}

	// Notes are session-local state and intentionally available during read-only
	// modes; they do not mutate the user's workspace.
	for _, name := range []string{"update_session_notes", "append_session_notes"} {
		m[name] = policy(ModeReadOnly, false, false, false, ToolCostLow)
	}
	m["switch_mode"] = policy(ModeReadOnly, true, true, false, ToolCostLow)
	// Todos are session-local state like notes: available in every mode,
	// safe for small models. todo_read is the read-only half of the pair.
	for _, name := range []string{"todo_write", "todo_read"} {
		m[name] = policy(ModeReadOnly, true, false, false, ToolCostLow)
	}
	m["run_shell"] = policy(ModeExploreShell, true, true, false, ToolCostHigh)
	m["shell_output"] = policy(ModeMutable, true, false, false, ToolCostLow)
	// job_kill terminates a process group or a sub-agent, like shell_output's
	// kill flag; job_list/job_output are plain reads (classified above).
	m["job_kill"] = policy(ModeMutable, true, false, false, ToolCostLow)
	// These tools combine read-only defaults with mutating optional actions, so
	// they remain visible for inspection but always pass through permission logic.
	m["git_branch"] = policy(ModeReadOnly, false, true, false, ToolCostLow)
	m["git_remote"] = policy(ModeReadOnly, false, true, true, ToolCostLow)

	for _, name := range []string{"write_file", "append_file", "edit_file", "delete_file", "make_directory"} {
		m[name] = leanMutate
	}
	for _, name := range []string{
		"move_file", "copy_file", "touch", "git_checkout", "git_pull", "git_push",
		"git_stash", "git_merge", "git_reset", "process_kill", "parallel_edit",
		"env_set",
	} {
		m[name] = mutate
	}
	for _, name := range []string{"git_add", "git_commit"} {
		m[name] = leanMutate
	}
	for _, name := range []string{"env_get", "env_list"} {
		m[name] = policy(ModeMutable, false, false, false, ToolCostLow)
	}
	// parallel_edit's isolated staging tools never touch the workspace directly.
	for _, name := range []string{"stage_edit", "stage_write", "stage_delete"} {
		m[name] = policy(ModeMutable, false, false, false, ToolCostLow)
	}

	setTimeout(m, inspectTimeout,
		"read_file", "list_directory", "find_files", "grep", "file_info",
		"get_working_directory", "git_status", "git_diff", "git_log", "git_branch",
		"find_symbol", "process_list", "disk_usage", "read_session_notes", "recall")
	setTimeout(m, mutateTimeout,
		"write_file", "edit_file", "append_file", "delete_file", "move_file",
		"copy_file", "make_directory", "touch", "git_add", "git_commit", "git_checkout",
		"git_stash", "git_merge", "git_reset", "git_remote", "process_kill",
		"update_session_notes", "append_session_notes", "remember", "forget")
	setTimeout(m, networkTimeout,
		"web_search", "web_search_api", "web_fetch", "web_crawl", "code_index", "semantic_search")
	setTimeout(m, longTimeout, "spawn_subagent", "parallel_edit")
	// The ceiling, for a caller holding only the name — ToolCallTimeout reads
	// the call's own timeout_sec instead.
	setTimeout(m, shellMaxTimeout+shellGrace, "run_shell")

	// Everything left is a default-budget tool. Doing it here rather than at
	// each lookup means a zero Timeout in the table is impossible, so nobody
	// can arm a context.WithTimeout(0) by forgetting a name.
	for name, p := range m {
		if p.Timeout <= 0 {
			p.Timeout = DefaultToolTimeout
			m[name] = p
		}
	}
	return m
}()

// setTimeout stamps a deadline class onto tools already in the table. Names not
// in it are skipped rather than created: a bare Timeout with no Modes would read
// as "allowed nowhere" and quietly shadow PolicyForName's conservative default.
func setTimeout(m map[string]ToolPolicy, d time.Duration, names ...string) {
	for _, name := range names {
		if p, ok := m[name]; ok {
			p.Timeout = d
			m[name] = p
		}
	}
}

// PolicyForName returns the built-in policy. Unknown tools default to the
// conservative external-tool posture: write/auto only, destructive, and not
// exposed to small models until explicitly classified.
func PolicyForName(name string) ToolPolicy {
	if p, ok := toolPolicies[name]; ok {
		return p
	}
	p := policy(ModeMutable, false, true, true, ToolCostHigh)
	p.Timeout = DefaultToolTimeout
	return p
}

// Permission effects, in precedence order: a deny anywhere in the ruleset wins
// outright, then ask, then allow. Precedence is fixed rather than first-match
// so that adding a broad allow rule can never silently widen past a narrow deny
// the user wrote earlier.
const (
	PermissionAllow = "allow"
	PermissionAsk   = "ask"
	PermissionDeny  = "deny"
)

// PermissionRule is one user-configured decision about a tool call, from
// config.json's "permissions". It lives beside the policy table because the
// same answer has to reach every caller — TUI dispatch, the headless Executor,
// and subagents — and those three had already drifted once over questions this
// table now answers in one place.
//
// Tool is a glob over the tool name ("write_file", "git_*", "*"). Path is a
// glob matched against the call's mutated paths, Cmd a glob matched against
// run_shell's command. A rule with neither Path nor Cmd matches every call to
// the named tool; a rule with one of them matches only when the call actually
// carries that kind of resource.
type PermissionRule struct {
	Tool   string `json:"tool"`
	Path   string `json:"path,omitempty"`
	Cmd    string `json:"cmd,omitempty"`
	Effect string `json:"effect"`
}

// EvaluatePermission resolves a call against the ruleset. matched=false means no
// rule applies and the caller keeps its built-in behavior (mode gating plus the
// approval prompt) unchanged — rules narrow or widen that default, they do not
// replace it.
func EvaluatePermission(rules []PermissionRule, call ToolCall) (effect string, matched bool) {
	found := ""
	for _, rule := range rules {
		if !ruleMatches(rule, call) {
			continue
		}
		switch rule.Effect {
		case PermissionDeny:
			return PermissionDeny, true // absolute; nothing outranks it
		case PermissionAsk:
			found = PermissionAsk
		case PermissionAllow:
			if found == "" {
				found = PermissionAllow
			}
		}
	}
	return found, found != ""
}

// PermissionRuleFor derives the rule an "always allow" answer writes for a call.
// run_shell widens to the command's first word, because a rule pinned to one
// exact command line would never match anything again; every other tool becomes
// a bare tool-name rule. Callers show the result to the user before saving it —
// this is a widening of the safety boundary and should never be silent.
func PermissionRuleFor(call ToolCall) PermissionRule {
	rule := PermissionRule{Tool: call.Function.Name, Effect: PermissionAllow}
	if call.Function.Name == "run_shell" {
		if fields := strings.Fields(shellCallCommand(call.Function.Arguments)); len(fields) > 0 {
			rule.Cmd = fields[0] + " *"
		}
	}
	return rule
}

// String renders a rule the way it is shown in the permission modal.
func (r PermissionRule) String() string {
	tool := r.Tool
	if tool == "" {
		tool = "*"
	}
	out := r.Effect + " " + tool
	if r.Cmd != "" {
		out += " cmd:" + r.Cmd
	}
	if r.Path != "" {
		out += " path:" + r.Path
	}
	return out
}

// Equal reports whether two rules express the same decision, so saving a rule
// twice does not grow the config file.
func (r PermissionRule) Equal(other PermissionRule) bool {
	return r.Tool == other.Tool && r.Path == other.Path && r.Cmd == other.Cmd && r.Effect == other.Effect
}

func ruleMatches(rule PermissionRule, call ToolCall) bool {
	if rule.Effect == "" {
		return false
	}
	pattern := strings.TrimSpace(rule.Tool)
	if pattern == "" {
		pattern = "*"
	}
	if !matchGlob(pattern, call.Function.Name) {
		return false
	}
	if cmd := strings.TrimSpace(rule.Cmd); cmd != "" {
		command := shellCallCommand(call.Function.Arguments)
		if command == "" || !matchCommand(cmd, command) {
			return false
		}
	}
	if p := strings.TrimSpace(rule.Path); p != "" {
		paths := MutatedPaths(call.Function.Name, call.Function.Arguments)
		if len(paths) == 0 {
			return false
		}
		hit := false
		for _, target := range paths {
			if matchPath(p, target) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// shellCallCommand pulls run_shell's command argument; "" for any other call.
func shellCallCommand(args json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(args, &a)
	return strings.TrimSpace(a.Command)
}

// matchGlob is path.Match over a value with no path structure (a tool name, a
// shell command line). A malformed pattern matches nothing rather than
// erroring, so a typo in config fails closed.
func matchGlob(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	ok, err := path.Match(pattern, s)
	return err == nil && ok
}

// matchCommand matches a shell command line. A trailing * is a prefix match on
// everything before it and an exact comparison otherwise. path.Match is wrong
// here: its * stops at a separator, so "npm *" would match "npm test" but not
// "npm run build --prefix ./web" — silently narrower than the rule reads, which
// is the dangerous direction for a rule to be misread in.
func matchCommand(pattern, command string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(command, prefix)
	}
	return pattern == command
}

// matchPath matches a path glob the way people actually write them: a trailing
// /** is a subtree prefix, a pattern with no separator matches the base name
// (so "*.env" catches any directory's .env), and anything else is path.Match
// against the slash-normalized path. path.Match alone would fail all three,
// because its * never crosses a separator.
func matchPath(pattern, target string) bool {
	target = filepath.ToSlash(filepath.Clean(target))
	pattern = filepath.ToSlash(pattern)
	if subtree, ok := strings.CutSuffix(pattern, "/**"); ok {
		return target == subtree || strings.HasPrefix(target, subtree+"/")
	}
	if !strings.Contains(pattern, "/") {
		if ok, err := path.Match(pattern, filepath.Base(target)); err == nil && ok {
			return true
		}
	}
	ok, err := path.Match(pattern, target)
	return err == nil && ok
}

// ToolCallTimeout is the deadline for one call, for every caller: TUI dispatch
// and the headless Executor. It used to be a name switch at each of those two
// sites, duplicating the classification this table already carries — so a newly
// registered tool got the default at one site and something else at the other.
func ToolCallTimeout(call ToolCall) time.Duration {
	// run_shell is the exception: the model picks its own budget per call, and
	// the handler kills at exactly that number.
	if call.Function.Name == "run_shell" {
		return shellCallTimeout(call.Function.Arguments) + shellGrace
	}
	return PolicyForName(call.Function.Name).Timeout
}

// shellCallTimeout is the budget the run_shell handler will enforce on itself
// for these arguments. Callers arming an outer deadline add shellGrace.
func shellCallTimeout(args json.RawMessage) time.Duration {
	var a struct {
		TimeoutSec float64 `json:"timeout_sec"`
	}
	_ = json.Unmarshal(args, &a)
	if a.TimeoutSec <= 0 {
		return shellDefaultTimeout
	}
	return min(time.Duration(a.TimeoutSec*float64(time.Second)), shellMaxTimeout)
}
