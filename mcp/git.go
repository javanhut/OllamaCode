package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func GitStatusTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_status",
			Description: "Show full git status: current branch, upstream tracking, staged/unstaged/untracked changes, and stash count. Use this to understand repo state before any git operations.",
			Parameters:  Schema{Type: "object", Properties: map[string]Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			if detectVCS() == "ivaldi" {
				// ivaldi status gives timeline + last seal + working-dir state
				// in one call; no need to replicate git's multi-call assembly.
				cmd := vcsExec(ctx, "git_status", "status")
				out, err := cmd.CombinedOutput()
				if err != nil {
					return string(out), err
				}
				return strings.TrimSpace(string(out)), nil
			}
			var out strings.Builder

			// Branch info
			cmd := exec.CommandContext(ctx, "git", "branch", "--show-current")
			b, _ := cmd.Output()
			branch := strings.TrimSpace(string(b))
			out.WriteString("branch: " + branch + "\n")

			// Tracking
			cmd = exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "@{upstream}")
			b, err := cmd.Output()
			if err == nil {
				out.WriteString("upstream: " + strings.TrimSpace(string(b)) + "\n")
			}

			// Ahead/behind
			cmd = exec.CommandContext(ctx, "git", "status", "-sb")
			b, _ = cmd.Output()
			sb := strings.TrimSpace(string(b))
			if idx := strings.IndexByte(sb, '\n'); idx >= 0 {
				sb = sb[idx+1:]
			}
			out.WriteString("status: " + sb + "\n")

			// Stash count
			cmd = exec.CommandContext(ctx, "git", "stash", "list")
			b, _ = cmd.Output()
			stashLines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(stashLines) > 0 && stashLines[0] != "" {
				out.WriteString("stashes: " + strconv.Itoa(len(stashLines)) + "\n")
			}

			// Short status
			out.WriteString("\nchanges:\n")
			cmd = exec.CommandContext(ctx, "git", "status", "-s")
			b, err = cmd.Output()
			if err != nil {
				return "", err
			}
			if len(b) == 0 {
				out.WriteString("(working tree clean)\n")
			} else {
				out.Write(b)
			}

			return out.String(), nil
		},
	}
}

func GitDiffTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_diff",
			Description: "Show git diff. By default shows unstaged changes. Use staged=true for staged changes. Use from_commit/to_commit to compare specific commits. Use path to filter to specific files or directories.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"staged":      {Type: "boolean", Description: "Show staged changes instead of unstaged."},
					"from_commit": {Type: "string", Description: "Base commit/branch for comparison (e.g. 'HEAD~3' or 'main')."},
					"to_commit":   {Type: "string", Description: "Target commit/branch. Defaults to working tree."},
					"path":        {Type: "string", Description: "Restrict diff to this file or directory."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Staged     bool   `json:"staged"`
				FromCommit string `json:"from_commit"`
				ToCommit   string `json:"to_commit"`
				Path       string `json:"path"`
			}
			json.Unmarshal(args, &a)
			argv := []string{"diff", "--color=never"}
			if a.Staged {
				argv = append(argv, "--staged")
			}
			if a.FromCommit != "" {
				if a.ToCommit != "" {
					argv = append(argv, a.FromCommit+"..."+a.ToCommit)
				} else {
					argv = append(argv, a.FromCommit)
				}
			}
			if a.Path != "" {
				argv = append(argv, "--", a.Path)
			}
			cmd := vcsExec(ctx, "git_diff", argv...)
			out, err := cmd.CombinedOutput()
			text := strings.TrimRight(string(out), "\n")
			if err != nil {
				if text != "" {
					return text, nil
				}
				return "", err
			}
			if text == "" {
				return "(no changes)", nil
			}
			return text, nil
		},
	}
}

func GitLogTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_log",
			Description: "Show git commit history with graph, author, and date. Filter by author, path, or date range. Use this to understand project history and find specific changes.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"count":  {Type: "number", Description: "Number of commits to show. Default 10, max 50."},
					"author": {Type: "string", Description: "Filter by author name or email."},
					"path":   {Type: "string", Description: "Only show commits touching this file/directory."},
					"since":  {Type: "string", Description: "Show commits more recent than this date (e.g. '2024-01-01' or '2 weeks ago')."},
					"grep":   {Type: "string", Description: "Filter commits whose message matches this pattern."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Count  int    `json:"count"`
				Author string `json:"author"`
				Path   string `json:"path"`
				Since  string `json:"since"`
				Grep   string `json:"grep"`
			}
			json.Unmarshal(args, &a)
			if a.Count <= 0 {
				a.Count = 10
			}
			if a.Count > 50 {
				a.Count = 50
			}

			argv := []string{"log", "-n", strconv.Itoa(a.Count), "--graph", "--color=never", "--format=%h %ad %an: %s", "--date=short"}
			if a.Author != "" {
				argv = append(argv, "--author="+a.Author)
			}
			if a.Since != "" {
				argv = append(argv, "--since="+a.Since)
			}
			if a.Grep != "" {
				argv = append(argv, "--grep="+a.Grep)
			}
			if a.Path != "" {
				argv = append(argv, "--", a.Path)
			}
			cmd := vcsExec(ctx, "git_log", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", err
			}
			return string(out), nil
		},
	}
}

func GitAddTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_add",
			Description: "Add file contents to the git index (git add).",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"paths": {Type: "string", Description: "File or directory paths to add. Use '.' for all."},
				},
				Required: []string{"paths"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Paths string `json:"paths"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			cmd := vcsExec(ctx, "git_add", "add", a.Paths)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return "added " + a.Paths, nil
		},
	}
}

func GitCommitTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_commit",
			Description: "Record changes to the git repository. Use all=true to auto-stage all modified/deleted files. Use amend=true to amend the previous commit (keep same message or provide a new one). Without flags, commits only previously staged changes.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"message": {Type: "string", Description: "The commit message."},
					"all":     {Type: "boolean", Description: "Automatically stage all modified and deleted files before committing."},
					"amend":   {Type: "boolean", Description: "Amend the previous commit instead of creating a new one."},
				},
				Required: []string{"message"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Message string `json:"message"`
				All     bool   `json:"all"`
				Amend   bool   `json:"amend"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			// For ivaldi, --all requires staging first (ivaldi seal doesn't
			// auto-stage). Handle that by running gather before seal.
			if a.All && detectVCS() == "ivaldi" {
				gatherCmd := vcsExec(ctx, "git_add", "gather", ".")
				if out, err := gatherCmd.CombinedOutput(); err != nil {
					return string(out), err
				}
			}
			argv := []string{"commit", "-m", a.Message}
			if a.All {
				argv = append(argv, "--all")
			}
			if a.Amend {
				argv = append(argv, "--amend")
			}
			cmd := vcsExec(ctx, "git_commit", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return string(out), nil
		},
	}
}

func GitBranchTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_branch",
			Description: "List, create, or delete git branches. With no arguments, lists all local branches (current marked with *). Use action='create' with a name to create a new branch. Use action='delete' to delete a branch (safe — refuses if not merged). Use remote=true to list remote branches.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"action": {Type: "string", Description: "Action: 'list' (default), 'create', or 'delete'."},
					"name":   {Type: "string", Description: "Branch name (required for create/delete)."},
					"remote": {Type: "boolean", Description: "List remote-tracking branches instead of local branches."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Action string `json:"action"`
				Name   string `json:"name"`
				Remote bool   `json:"remote"`
			}
			json.Unmarshal(args, &a)
			if a.Action == "" {
				a.Action = "list"
			}
			switch a.Action {
			case "create":
				if a.Name == "" {
					return "", fmt.Errorf("name is required for create")
				}
				cmd := vcsExec(ctx, "git_branch", "branch", a.Name)
				out, err := cmd.CombinedOutput()
				if err != nil {
					return string(out), err
				}
				return "created branch " + a.Name, nil
			case "delete":
				if a.Name == "" {
					return "", fmt.Errorf("name is required for delete")
				}
				cmd := vcsExec(ctx, "git_branch", "branch", "-d", a.Name)
				out, err := cmd.CombinedOutput()
				if err != nil {
					return string(out), err
				}
				return string(out), nil
			default:
				argv := []string{"branch"}
				if a.Remote {
					argv = append(argv, "-r")
				}
				cmd := vcsExec(ctx, "git_branch", argv...)
				out, _ := cmd.CombinedOutput()
				return string(out), nil
			}
		},
	}
}

func GitCheckoutTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_checkout",
			Description: "Switch to a branch, create-and-switch to a new one, or restore files. Use new_branch=true with target to create a branch and switch to it. Use target with a file path to restore that file from HEAD.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"target":     {Type: "string", Description: "Branch name to switch to, or file path to restore."},
					"new_branch": {Type: "boolean", Description: "Create a new branch with the given target name before switching."},
				},
				Required: []string{"target"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Target    string `json:"target"`
				NewBranch bool   `json:"new_branch"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Target == "" {
				return "", fmt.Errorf("target is required")
			}
			// ivaldi checkout-to-file is not generally supported. If the
			// target is an existing file and we're on ivaldi, explain.
			if detectVCS() == "ivaldi" && !a.NewBranch {
				if _, err := os.Stat(a.Target); err == nil {
					return "", fmt.Errorf("ivaldi does not support restoring individual files via checkout; use 'ivaldi rewind --discard <seal>' to restore the working tree to a specific seal")
				}
			}
			argv := []string{"checkout"}
			if a.NewBranch {
				argv = append(argv, "-b")
			}
			argv = append(argv, a.Target)
			cmd := vcsExec(ctx, "git_checkout", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return string(out), nil
		},
	}
}

func GitPullTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_pull",
			Description: "Fetch from and integrate with a remote repository. Uses rebase by default for a clean linear history. Specify remote and branch, or defaults to the current upstream.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"remote": {Type: "string", Description: "Remote name. Defaults to 'origin'."},
					"branch": {Type: "string", Description: "Remote branch. Defaults to current tracking branch."},
					"rebase": {Type: "boolean", Description: "Use rebase instead of merge. Default true."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Remote string `json:"remote"`
				Branch string `json:"branch"`
				Rebase *bool  `json:"rebase"`
			}
			json.Unmarshal(args, &a)
			if a.Remote == "" {
				a.Remote = "origin"
			}
			rebase := true
			if a.Rebase != nil {
				rebase = *a.Rebase
			}
			argv := []string{"pull"}
			if rebase {
				argv = append(argv, "--rebase")
			}
			if a.Branch != "" {
				argv = append(argv, a.Remote, a.Branch)
			} else {
				argv = append(argv, a.Remote)
			}
			cmd := vcsExec(ctx, "git_pull", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return string(out), nil
		},
	}
}

func GitPushTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_push",
			Description: "Push commits to a remote repository. By default pushes current branch to the same-named remote branch. Use set_upstream=true on first push of a new branch. Uses --force-with-lease for safer force push.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"remote":       {Type: "string", Description: "Remote name. Defaults to 'origin'."},
					"branch":       {Type: "string", Description: "Remote branch name. Defaults to current branch name."},
					"set_upstream": {Type: "boolean", Description: "Set remote as upstream (-u). Use this for first push of a new branch."},
					"force":        {Type: "boolean", Description: "Use --force-with-lease. Safer than hard force push, but still overwrites history."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Remote      string `json:"remote"`
				Branch      string `json:"branch"`
				SetUpstream bool   `json:"set_upstream"`
				Force       bool   `json:"force"`
			}
			json.Unmarshal(args, &a)
			if a.Remote == "" {
				a.Remote = "origin"
			}
			argv := []string{"push"}
			if a.SetUpstream {
				argv = append(argv, "-u")
			}
			if a.Force {
				argv = append(argv, "--force-with-lease")
			}
			argv = append(argv, a.Remote)
			if a.Branch != "" {
				argv = append(argv, a.Branch)
			}
			cmd := vcsExec(ctx, "git_push", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return string(out), nil
		},
	}
}

func GitStashTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_stash",
			Description: "Stash working changes to switch contexts. Actions: 'push' (save changes, default), 'pop' (restore and remove latest), 'list' (show all stashes), 'drop' (delete latest). Optional message to label the stash.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"action":  {Type: "string", Description: "Action: 'push' (default), 'pop', 'list', or 'drop'."},
					"message": {Type: "string", Description: "Optional description for the stash (push only)."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			if isVCSUnsupported("git_stash") {
				return "ivaldi has no stash equivalent. Use timelines (branches) to switch contexts without committing: create one with git_branch (action=create), or see `ivaldi timeline --help`.", nil
			}
			var a struct {
				Action  string `json:"action"`
				Message string `json:"message"`
			}
			json.Unmarshal(args, &a)
			if a.Action == "" {
				a.Action = "push"
			}
			var cmd *exec.Cmd
			switch a.Action {
			case "pop":
				cmd = exec.CommandContext(ctx, "git", "stash", "pop")
			case "list":
				cmd = exec.CommandContext(ctx, "git", "stash", "list")
			case "drop":
				cmd = exec.CommandContext(ctx, "git", "stash", "drop")
			default:
				argv := []string{"stash", "push"}
				if a.Message != "" {
					argv = append(argv, "-m", a.Message)
				}
				cmd = exec.CommandContext(ctx, "git", argv...)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			text := strings.TrimSpace(string(out))
			if text == "" {
				if a.Action == "list" {
					return "(no stashes)", nil
				}
				return "[ok]", nil
			}
			return text, nil
		},
	}
}

func GitMergeTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_merge",
			Description: "Merge a branch into the current branch. Use no_ff=true to always create a merge commit. Returns the merge result or conflict information.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"branch": {Type: "string", Description: "Branch to merge into current branch."},
					"no_ff":  {Type: "boolean", Description: "Create a merge commit even if fast-forward is possible. Default false."},
				},
				Required: []string{"branch"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Branch string `json:"branch"`
				NoFF   bool   `json:"no_ff"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Branch == "" {
				return "", fmt.Errorf("branch is required")
			}
			argv := []string{"merge"}
			if a.NoFF {
				argv = append(argv, "--no-ff")
			}
			argv = append(argv, a.Branch)
			cmd := vcsExec(ctx, "git_merge", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return strings.TrimSpace(string(out)), nil
		},
	}
}

func GitResetTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_reset",
			Description: "Unstage files or reset HEAD. With a path, unstages that file from the index. With a commit ref, resets HEAD to that commit. Modes: 'soft' (keep changes staged), 'mixed' (keep changes unstaged - default), 'hard' (DESTRUCTIVE: discard changes).",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"target": {Type: "string", Description: "File path to unstage, or commit ref to reset to (e.g. 'HEAD~1')."},
					"mode":   {Type: "string", Description: "Reset mode: 'soft', 'mixed' (default), or 'hard'. Only applies to commit resets."},
				},
				Required: []string{"target"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Target string `json:"target"`
				Mode   string `json:"mode"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Target == "" {
				return "", fmt.Errorf("target is required")
			}
			if a.Mode == "" {
				a.Mode = "mixed"
			}
			isFile := false
			if info, err := os.Stat(a.Target); err == nil && !info.IsDir() {
				isFile = true
			}
			var argv []string
			if isFile {
				argv = []string{"reset", "HEAD", "--", a.Target}
			} else {
				argv = []string{"reset"}
				switch a.Mode {
				case "hard":
					argv = append(argv, "--hard")
				case "soft":
					argv = append(argv, "--soft")
				default:
				}
				argv = append(argv, a.Target)
			}
			cmd := vcsExec(ctx, "git_reset", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			text := strings.TrimSpace(string(out))
			if text == "" {
				return "[ok]", nil
			}
			return text, nil
		},
	}
}

func GitRemoteTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_remote",
			Description: "Manage remote repositories. With no arguments, lists all remotes with their URLs (-v). Use action='add' to add a new remote, action='remove' to delete one, or action='show' to inspect a specific remote.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"action": {Type: "string", Description: "Action: 'list' (default), 'add', 'remove', or 'show'."},
					"name":   {Type: "string", Description: "Remote name (required for add/remove/show)."},
					"url":    {Type: "string", Description: "Remote URL (required for add)."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Action string `json:"action"`
				Name   string `json:"name"`
				URL    string `json:"url"`
			}
			json.Unmarshal(args, &a)
			if a.Action == "" {
				a.Action = "list"
			}
			var argv []string
			switch a.Action {
			case "add":
				if a.Name == "" || a.URL == "" {
					return "", fmt.Errorf("name and url are required for add")
				}
				argv = []string{"remote", "add", a.Name, a.URL}
			case "remove":
				if a.Name == "" {
					return "", fmt.Errorf("name is required for remove")
				}
				argv = []string{"remote", "remove", a.Name}
			case "show":
				if a.Name == "" {
					return "", fmt.Errorf("name is required for show")
				}
				argv = []string{"remote", "show", a.Name}
			default:
				argv = []string{"remote", "-v"}
			}
			cmd := vcsExec(ctx, "git_remote", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			text := strings.TrimSpace(string(out))
			if text == "" {
				return "[ok]", nil
			}
			return text, nil
		},
	}
}
