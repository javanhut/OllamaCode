package tools

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
}

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
		"disk_usage", "spawn_subagent",
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
	m["todo_write"] = policy(ModeReadOnly, true, false, false, ToolCostLow)
	m["run_shell"] = policy(ModeExploreShell, true, true, false, ToolCostHigh)
	m["shell_output"] = policy(ModeMutable, true, false, false, ToolCostLow)
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
	return m
}()

// PolicyForName returns the built-in policy. Unknown tools default to the
// conservative external-tool posture: write/auto only, destructive, and not
// exposed to small models until explicitly classified.
func PolicyForName(name string) ToolPolicy {
	if p, ok := toolPolicies[name]; ok {
		return p
	}
	return policy(ModeMutable, false, true, true, ToolCostHigh)
}
