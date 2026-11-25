package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/javanhut/ollama_code/tools"
)

// Approval policy: one decision procedure for every entry point. The TUI asks
// a human when the answer is Ask; a non-interactive run (ocode -p, a spawned
// subagent, eval) has nobody to ask, so Ask is refused there. Before this
// existed the rules lived in the TUI's prompt code alone, and a headless run or
// a subagent could rewrite AGENTS.md or act on a fetched page's instructions
// with no check at all — the same boundary, enforced at one door of three.

// Decision is the outcome of Decide.
type Decision int

const (
	Allow Decision = iota
	Ask
	Deny
)

func (d Decision) String() string {
	return [...]string{"allow", "ask", "deny"}[d]
}

// ApprovalInput is everything Decide looks at. Callers fill what they know;
// zero values are the conservative defaults.
type ApprovalInput struct {
	Call   tools.ToolCall
	Policy tools.ToolPolicy
	// ConfigRules are the user's config.json rules: deny > ask > allow, and an
	// explicit allow is the only thing that waives the protected-path and
	// untrusted-content prompts.
	ConfigRules []tools.PermissionRule
	// SessionRules are "s" grants from the interactive prompt (allow only).
	SessionRules []tools.PermissionRule
	// AllowAllBatch is the interactive "a" key for the current batch.
	AllowAllBatch bool
	// Auto is auto mode (or a non-interactive run, which is auto mode with
	// nobody at the keyboard): destructive calls inside the workspace proceed.
	Auto bool
	// Untrusted: content from the web or an MCP server entered this turn.
	Untrusted bool
	// InWorkspace reports whether a path argument stays inside the workspace.
	// Nil treats every path as inside (the tools layer's jail still applies).
	InWorkspace func(path string) bool
}

// PathArgKeys are the argument names the file tools use for target paths.
var PathArgKeys = []string{"path", "dest", "destination", "new_path", "to", "source", "src", "working_dir"}

// Decide applies the approval policy. reason is set for Ask and Deny.
func Decide(in ApprovalInput) (Decision, string) {
	if effect, ok := tools.EvaluatePermission(in.ConfigRules, in.Call); ok {
		switch effect {
		case tools.PermissionDeny:
			return Deny, "denied by a permission rule in the user's config"
		case tools.PermissionAllow:
			return Allow, ""
		default:
			return Ask, "an ask rule in the user's config matches this call"
		}
	}
	if !in.Policy.Destructive {
		return Allow, ""
	}
	// Agent/editor config and git hooks are write-then-execute targets: a
	// write there is obeyed or run later by something trusted (the next
	// session's instructions, git, the editor). Only an explicit config rule
	// above may waive this — not a session grant, not "a", not auto mode.
	if TouchesProtectedPath(in.Call) {
		return Ask, "the call writes agent/editor configuration or git hooks, which later sessions or tools obey"
	}
	// Session grants were given for the user's intent; after untrusted
	// content, a call may be carrying out the content's intent instead
	// (Meta's Rule of Two: untrusted input plus a state change needs a human).
	if !in.Untrusted {
		if effect, ok := tools.EvaluatePermission(in.SessionRules, in.Call); ok && effect == tools.PermissionAllow {
			return Allow, ""
		}
	}
	if in.AllowAllBatch {
		return Allow, ""
	}
	if !in.Auto {
		return Ask, "destructive call"
	}
	if in.Untrusted {
		return Ask, "untrusted web or MCP content entered the conversation this turn"
	}
	if in.InWorkspace != nil {
		for _, p := range pathArgs(in.Call) {
			if !in.InWorkspace(p) {
				return Ask, "the call reaches outside the workspace"
			}
		}
	}
	return Allow, ""
}

// TouchesProtectedPath reports whether a call names a write-protected config
// path (tools.IsProtectedPath) in any target-path argument.
func TouchesProtectedPath(call tools.ToolCall) bool {
	return slices.ContainsFunc(pathArgs(call), tools.IsProtectedPath)
}

// pathArgs extracts the target paths from a call: the top-level path keys
// (working_dir excluded — running a command FROM a directory writes nothing
// there by itself) plus multi_edit-style nested edits.
func pathArgs(call tools.ToolCall) []string {
	var args map[string]any
	if err := json.Unmarshal(call.Function.Arguments, &args); err != nil {
		return nil
	}
	var out []string
	for _, key := range PathArgKeys {
		if p, ok := args[key].(string); ok && p != "" && key != "working_dir" {
			out = append(out, p)
		}
	}
	if edits, ok := args["edits"].([]any); ok {
		for _, e := range edits {
			if em, ok := e.(map[string]any); ok {
				if p, ok := em["path"].(string); ok && p != "" {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// untrustedMarker is the framing tools.wrapUntrustedContent puts around web
// and MCP output.
const untrustedMarker = "UNTRUSTED EXTERNAL CONTENT"

// WebContentTools pull attacker-controlled bytes into the conversation; any
// tool whose policy reaches the network (every MCP tool) counts as well.
var WebContentTools = map[string]bool{
	"web_fetch": true, "web_search": true, "web_search_api": true, "web_crawl": true,
}

// IsContentTool: a name check as well as the marker, so read_file on a file
// that merely mentions the marker text does not taint the turn.
func IsContentTool(name string, policy tools.ToolPolicy) bool {
	return WebContentTools[name] || policy.Network
}

// ResultIsUntrusted reports whether a content tool's result carried untrusted
// framing (a failed fetch returns an error without it and does not taint).
func ResultIsUntrusted(name string, policy tools.ToolPolicy, result string) bool {
	return IsContentTool(name, policy) && strings.Contains(result, untrustedMarker)
}

// approvalState is a non-interactive run's taint flag. A run is one turn, so
// it never resets.
type approvalState struct{ untrusted atomic.Bool }
