package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

func RunShellTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "run_shell",
			Description: "Run a shell command via `sh -c`. Use for awk, sed, find, complex pipelines, or anything not covered by a dedicated tool. Returns combined stdout+stderr. Supports stdin input via the stdin parameter. Non-zero exits are reported in the result. Default timeout 30s, max 300s; a foreground command that exceeds the timeout is killed. For long-running or never-terminating commands — dev servers, file watchers, `tail -f`, builds you want to keep running — set background=true: the command starts detached and this returns immediately with a job id, so the turn isn't blocked. Read its output or stop it later with shell_output.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"command":     {Type: "string", Description: "The shell command to execute."},
					"working_dir": {Type: "string", Description: "Directory to run in. Defaults to the current working directory."},
					"timeout_sec": {Type: "number", Description: "Hard timeout in seconds for foreground runs. Defaults to 30, max 300. Ignored when background=true."},
					"stdin":       {Type: "string", Description: "Text to pipe into the command's standard input."},
					"background":  {Type: "boolean", Description: "Run detached and return immediately with a job id instead of waiting. Use for commands that run for a while or never exit."},
				},
				Required: []string{"command"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Command    string `json:"command"`
				WorkingDir string `json:"working_dir"`
				Stdin      string `json:"stdin"`
				Background bool   `json:"background"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if strings.TrimSpace(a.Command) == "" {
				return "", fmt.Errorf("command is required")
			}
			// The command string itself is not confinable, but the directory it
			// starts in is — don't let working_dir teleport the shell elsewhere.
			if a.WorkingDir != "" {
				if err := jailCheck(a.WorkingDir); err != nil {
					return "", err
				}
			}
			if a.Background {
				job, err := startBackgroundShell(a.Command, a.WorkingDir, a.Stdin)
				if err != nil {
					return "", err
				}
				return withSandboxNotice(fmt.Sprintf("started background job %d (pid %d): %s\nRead its output with shell_output({\"job\": %d}); stop it with shell_output({\"job\": %d, \"kill\": true}).",
					job.id, job.pid, shortCommand(a.Command), job.id, job.id)), nil
			}
			// One copy of the default/cap arithmetic, shared with the deadline
			// every caller arms on the outside (tools.ToolCallTimeout).
			res, err := runShellCommand(ctx, a.Command, a.WorkingDir, a.Stdin, shellCallTimeout(args))
			return withSandboxNotice(res), err
		},
	}
}

// AskUserQuestion is a parsed ask_user call. The TUI renders the options as a
// selectable list and the handler renders them into the model-facing text, so
// both read the arguments through ParseAskUser rather than each doing their own
// decoding — a picker offering different options than the model believes it
// asked about is worse than no picker.
type AskUserQuestion struct {
	Question    string
	Options     []string
	MultiSelect bool
	Recommended string
}

// ParseAskUser decodes an ask_user call's arguments. `options` is an array, but
// a pipe-separated string is accepted too: that was the old schema, and a small
// model handed an array schema will still sometimes send the string. Salvaging
// it here costs four lines and saves a wasted turn. The salvaged form stays
// single-select — it has no way to carry multi_select or recommended.
func ParseAskUser(args json.RawMessage) AskUserQuestion {
	var a struct {
		Question    string          `json:"question"`
		Options     json.RawMessage `json:"options"`
		MultiSelect bool            `json:"multi_select"`
		Recommended string          `json:"recommended"`
	}
	_ = json.Unmarshal(args, &a)
	out := AskUserQuestion{Question: strings.TrimSpace(a.Question), MultiSelect: a.MultiSelect}

	var list []string
	if err := json.Unmarshal(a.Options, &list); err != nil {
		var joined string
		if err := json.Unmarshal(a.Options, &joined); err == nil {
			list = strings.Split(joined, "|")
		}
	}
	for _, opt := range list {
		if opt = strings.TrimSpace(opt); opt != "" {
			out.Options = append(out.Options, opt)
		}
	}
	// A recommended label that names no option would mark a row that does not
	// exist; drop it rather than render a phantom recommendation.
	if rec := strings.TrimSpace(a.Recommended); rec != "" {
		if slices.Contains(out.Options, rec) {
			out.Recommended = rec
		}
	}
	return out
}

func AskUserTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "ask_user",
			Description: "Ask the user a question when you need clarification before proceeding. Use this for: confirming destructive operations, choosing between multiple approaches, getting missing context, or when you're stuck. Supply options whenever the answer is a choice — they are shown as a list the user picks from with one keypress, so you get back exactly one of your own labels instead of free-form prose to interpret. The user's pick is delivered as this tool's result (ANSWER: <label>). Set multi_select=true when several options may apply at once; the result then lists every chosen label. Set recommended to the option you would pick — it is listed first and marked, but the user can still choose anything else. After calling this, STOP and wait — the answer arrives as the tool result.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"question": {Type: "string", Description: "The question to ask the user. Be specific and include context so they can give a quick answer."},
					"options": {
						Type:        "array",
						Description: "Suggested answers, e.g. [\"yes\", \"no\", \"show me an example\"]. Keep each one short and distinct — the user picks one by number. Omit for an open question.",
						Items:       &Property{Type: "string"},
					},
					"multi_select": {Type: "boolean", Description: "Let the user pick several options instead of exactly one. Default false. The tool result joins the chosen labels with \", \"."},
					"recommended":  {Type: "string", Description: "The option you recommend, copied exactly from options. Shown first and marked as recommended. Ignored if it does not match one of options."},
				},
				Required: []string{"question"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			a := ParseAskUser(args)
			msg := "QUESTION: " + a.Question
			if len(a.Options) > 0 {
				msg += "\nOptions: [" + strings.Join(a.Options, " | ") + "]"
			}
			msg += "\n\n(Stop here and wait for the user to answer before continuing.)"
			return msg, nil
		},
	}
}

func ProcessListTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "process_list",
			Description: "List running processes sorted by memory usage. Returns PID, CPU%, MEM%, and command. Filter by name to find specific processes. Use this to check if servers, builds, or background tasks are still running.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"name":  {Type: "string", Description: "Filter to processes whose command contains this substring (case-insensitive)."},
					"limit": {Type: "number", Description: "Maximum number of results. Default 20."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Name  string `json:"name"`
				Limit int    `json:"limit"`
			}
			json.Unmarshal(args, &a)
			if a.Limit <= 0 {
				a.Limit = 20
			}
			argv := []string{"aux", "--sort=-%mem"}
			cmd := exec.CommandContext(ctx, "ps", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("ps failed: %w", err)
			}
			lines := strings.Split(string(out), "\n")
			header := lines[0]
			var matches []string
			for _, line := range lines[1:] {
				if line == "" {
					continue
				}
				if a.Name != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(a.Name)) {
					continue
				}
				matches = append(matches, line)
				if len(matches) >= a.Limit {
					break
				}
			}
			if len(matches) == 0 {
				if a.Name != "" {
					return fmt.Sprintf("no processes matching %q found", a.Name), nil
				}
				return header + "\n(no user processes)", nil
			}
			return header + "\n" + strings.Join(matches, "\n"), nil
		},
	}
}

func ProcessKillTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "process_kill",
			Description: "Kill a running process by PID or name. Default signal is 15 (SIGTERM - graceful). Use signal=9 for SIGKILL (force). Use by_name=true to kill all processes matching a name via pkill.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"target":  {Type: "string", Description: "PID or process name to kill."},
					"signal":  {Type: "number", Description: "Signal number. Default 15 (SIGTERM), use 9 for SIGKILL."},
					"by_name": {Type: "boolean", Description: "Treat target as a process name (uses pkill) instead of PID."},
				},
				Required: []string{"target"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Target string `json:"target"`
				Signal int    `json:"signal"`
				ByName bool   `json:"by_name"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Target == "" {
				return "", fmt.Errorf("target is required")
			}
			if a.Signal <= 0 {
				a.Signal = 15
			}
			var cmd *exec.Cmd
			if a.ByName {
				cmd = exec.CommandContext(ctx, "pkill", "-"+strconv.Itoa(a.Signal), a.Target)
			} else {
				cmd = exec.CommandContext(ctx, "kill", "-"+strconv.Itoa(a.Signal), a.Target)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), fmt.Errorf("kill failed: %w", err)
			}
			if a.ByName {
				return "sent signal " + strconv.Itoa(a.Signal) + " to processes matching " + a.Target, nil
			}
			return "sent signal " + strconv.Itoa(a.Signal) + " to PID " + a.Target, nil
		},
	}
}

func DiskUsageTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "disk_usage",
			Description: "Show disk usage for filesystems or directories. Omit path to see all mounted filesystems (df -h). Provide a path to see directory usage (du). Use max_depth to control how deep du recurses. Use this to check available space before large operations.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":      {Type: "string", Description: "Directory or mount point to check. If omitted, shows all real filesystems."},
					"max_depth": {Type: "number", Description: "For directory size analysis, how deep to summarize. Default 1 (immediate children)."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path     string `json:"path"`
				MaxDepth int    `json:"max_depth"`
			}
			json.Unmarshal(args, &a)
			if a.Path == "" {
				cmd := exec.CommandContext(ctx, "df", "-x", "tmpfs", "-x", "devtmpfs", "-x", "squashfs", "-h")
				out, err := cmd.CombinedOutput()
				if err != nil {
					return "", err
				}
				return string(out), nil
			}
			if a.MaxDepth <= 0 {
				a.MaxDepth = 1
			}
			cmd := exec.CommandContext(ctx, "du", "-h", "--max-depth="+strconv.Itoa(a.MaxDepth), a.Path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(out)), nil
		},
	}
}
