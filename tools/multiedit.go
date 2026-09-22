package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// multi_edit applies several old_string/new_string replacements to ONE file as
// a single atomic change. A model making four related edits to a file otherwise
// spends four tool calls, four permission prompts and four syntax gates — and
// the gate can reject edit 2 of 4 because the file is only valid again after
// edit 3. Here the edits are applied in order, in memory, through the same
// tiered matcher as edit_file; the syntax gate and the formatter run once on
// the final result, and nothing is written unless every edit matched.
//
// Line-range edits are deliberately absent: after the first edit every later
// line number would be off by that edit's delta, which is exactly the kind of
// arithmetic a model gets wrong.

// multiEditItem is one replacement inside a multi_edit call.
type multiEditItem struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

type multiEditArgs struct {
	Path  string          `json:"path"`
	Edits []multiEditItem `json:"edits"`
}

// resolveMultiEdit computes what multi_edit would write, without writing it.
// Shared by the handler and the approval preview for the same reason
// resolveEdit is: the modal must describe the change that actually lands.
func resolveMultiEdit(a multiEditArgs) (oldContent, updated string, counts []int, maxTier int, err error) {
	if a.Path == "" {
		return "", "", nil, 0, fmt.Errorf("path is required")
	}
	if len(a.Edits) == 0 {
		return "", "", nil, 0, fmt.Errorf("edits is empty: pass at least one {old_string, new_string} object")
	}
	if err := jailCheck(a.Path); err != nil {
		return "", "", nil, 0, err
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", "", nil, 0, err
	}
	updated = string(data)
	counts = make([]int, len(a.Edits))
	for i, e := range a.Edits {
		if e.OldString == "" {
			return "", "", nil, 0, fmt.Errorf("edit %d of %d: old_string is empty. No changes were written", i+1, len(a.Edits))
		}
		if e.OldString == e.NewString {
			return "", "", nil, 0, fmt.Errorf("edit %d of %d: old_string and new_string are identical. No changes were written", i+1, len(a.Edits))
		}
		next, count, tier, editErr := applyOneEdit(updated, editArgs{
			Path: a.Path, OldString: e.OldString, NewString: e.NewString, ReplaceAll: e.ReplaceAll,
		})
		if editErr != nil {
			hint := ""
			if i > 0 {
				hint = " Edits apply in order, so old_string must match the file AFTER the earlier edits in this call."
			}
			return "", "", nil, 0, fmt.Errorf("edit %d of %d failed: %v\nNo changes were written — the whole multi_edit was rejected.%s", i+1, len(a.Edits), editErr, hint)
		}
		updated = next
		counts[i] = count
		maxTier = max(maxTier, tier)
	}
	if verifyBytes(a.Path, data) == nil {
		if verr := verifyBytes(a.Path, []byte(updated)); verr != nil {
			return "", "", nil, 0, fmt.Errorf("multi_edit rejected: the combined edits would introduce a syntax error in %s: %v\nNo changes were written — fix the edits and retry", a.Path, verr)
		}
	}
	updated = string(formatBytes(a.Path, []byte(updated)))
	return string(data), updated, counts, maxTier, nil
}

// PreviewMultiEdit renders the diff multi_edit would apply; ok=false when the
// edits do not resolve against the file on disk.
func PreviewMultiEdit(path string, args json.RawMessage) (diff string, ok bool) {
	a, err := decodeMultiEditArgs(args)
	if err != nil {
		return "", false
	}
	if path != "" {
		a.Path = path
	}
	oldContent, updated, _, _, err := resolveMultiEdit(a)
	if err != nil {
		return "", false
	}
	diff = unifiedDiff(oldContent, updated, a.Path)
	if strings.HasPrefix(diff, "(diff omitted") {
		return "", false
	}
	return diff, diff != ""
}

// decodeMultiEditArgs tolerates `edits` double-encoded as a JSON string, which
// weak models emit often enough to matter. Registry.Invoke already unwraps it
// via NormalizeArgs; the preview reads the raw call, so it unwraps too.
func decodeMultiEditArgs(raw json.RawMessage) (multiEditArgs, error) {
	var loose struct {
		Path  string          `json:"path"`
		Edits json.RawMessage `json:"edits"`
	}
	if err := json.Unmarshal(raw, &loose); err != nil {
		return multiEditArgs{}, fmt.Errorf("invalid arguments: %w", err)
	}
	a := multiEditArgs{Path: loose.Path}
	edits := loose.Edits
	if jsonKind(edits) == "string" {
		edits, _ = unwrapStringified(edits)
	}
	if len(edits) > 0 && jsonKind(edits) != "null" {
		if err := json.Unmarshal(edits, &a.Edits); err != nil {
			return multiEditArgs{}, fmt.Errorf("invalid arguments: edits must be an array of {old_string, new_string, replace_all} objects: %w", err)
		}
	}
	return a, nil
}

func MultiEditTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "multi_edit",
			Description: "Apply several text replacements to ONE file in a single atomic call. Edits apply in order, each matched like edit_file (exact first, then whitespace-tolerant); a later edit sees the file as changed by the earlier ones. If any edit fails to match, nothing is written. Prefer this over repeated edit_file calls when changing several places in the same file.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the file."},
					"edits": {
						Type:        "array",
						Description: "Replacements to apply in order.",
						Items: &Property{
							Type: "object",
							Properties: map[string]Property{
								"old_string":  {Type: "string", Description: "Text currently in the file (after any earlier edits in this call)."},
								"new_string":  {Type: "string", Description: "Replacement text."},
								"replace_all": {Type: "boolean", Description: "Replace every occurrence. Default false."},
							},
							Required: []string{"old_string", "new_string"},
						},
					},
				},
				Required: []string{"path", "edits"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			a, err := decodeMultiEditArgs(args)
			if err != nil {
				return "", err
			}
			data, updated, counts, maxTier, err := resolveMultiEdit(a)
			if err != nil {
				return "", err
			}
			mode := os.FileMode(0o644)
			if info, err := os.Stat(a.Path); err == nil {
				mode = info.Mode().Perm()
			}
			if err := WriteFileAtomic(a.Path, []byte(updated), mode); err != nil {
				return "", err
			}
			hash, _ := FileHash(a.Path)
			total := 0
			for _, c := range counts {
				total += c
			}
			tierNote := ""
			switch maxTier {
			case 2:
				tierNote = " (some edits matched after whitespace-normalization; copy exact text next time)"
			case 3:
				tierNote = " (some edits matched by fuzzy similarity — verify the diff carefully)"
			}
			result := fmt.Sprintf("edited %s: applied %d edit(s), %d replacement(s)%s\nNew Hash: %s", a.Path, len(counts), total, tierNote, hash)
			if diff := unifiedDiff(data, updated, a.Path); diff != "" {
				result += "\n" + diff
			}
			return result + postEditDiagnostics(ctx, a.Path), nil
		},
	}
}
