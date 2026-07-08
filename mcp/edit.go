package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// leadingWS returns the leading run of spaces/tabs in s.
func leadingWS(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// reindentBlock rebases the indentation of newStr from oldIndent to fileIndent,
// preserving each line's relative indentation. Used when a whitespace-normalized
// match applies the model's replacement to the file's actual indentation.
func reindentBlock(newStr, oldIndent, fileIndent string) string {
	if oldIndent == fileIndent {
		return newStr
	}
	lines := strings.Split(newStr, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		lines[i] = fileIndent + strings.TrimPrefix(ln, oldIndent)
	}
	return strings.Join(lines, "\n")
}

// applyEdit replaces oldStr with newStr in content using a tiered matcher:
//
//	tier 1: exact substring match (whitespace must match exactly)
//	tier 2: whitespace-normalized, line-based match (tolerates indentation and
//	        CRLF/LF differences; re-indents newStr to the file's real indentation)
//
// It returns the updated content, the number of replacements, and the tier that
// matched. Tier 2 is only attempted when tier 1 finds nothing.
func applyEdit(content, oldStr, newStr string, replaceAll bool) (updated string, count, tier int, err error) {
	// Tier 1: exact.
	if c := strings.Count(content, oldStr); c > 0 {
		if c > 1 && !replaceAll {
			return "", c, 1, fmt.Errorf("old_string appears %d times; pass replace_all=true or use a more specific snippet", c)
		}
		if replaceAll {
			return strings.ReplaceAll(content, oldStr, newStr), c, 1, nil
		}
		return strings.Replace(content, oldStr, newStr, 1), c, 1, nil
	}

	// Tier 2: whitespace-normalized line match.
	crlf := strings.Count(content, "\r\n")
	useCRLF := crlf > (strings.Count(content, "\n") - crlf)
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	oldNorm := strings.ReplaceAll(oldStr, "\r\n", "\n")
	newNorm := strings.ReplaceAll(newStr, "\r\n", "\n")

	cLines := strings.Split(norm, "\n")
	oLines := strings.Split(oldNorm, "\n")
	for len(oLines) > 1 && strings.TrimSpace(oLines[len(oLines)-1]) == "" {
		oLines = oLines[:len(oLines)-1]
	}
	k := len(oLines)
	if k == 0 {
		return "", 0, 2, fmt.Errorf("old_string is empty")
	}

	// Find all window start indices whose trimmed lines match oLines exactly.
	var starts []int
	if k <= len(cLines) {
		for i := 0; i+k <= len(cLines); i++ {
			match := true
			for j := range k {
				if strings.TrimSpace(cLines[i+j]) != strings.TrimSpace(oLines[j]) {
					match = false
					break
				}
			}
			if match {
				starts = append(starts, i)
			}
		}
	}

	if len(starts) == 0 {
		// Tier 3: fuzzy line-block match.
		return fuzzyEdit(cLines, oLines, newNorm, useCRLF)
	}
	if len(starts) > 1 && !replaceAll {
		return "", len(starts), 2, fmt.Errorf("old_string matches %d locations (after whitespace-normalization); pass replace_all=true or use a more specific snippet", len(starts))
	}

	// Apply replacements from the bottom up so earlier indices stay valid.
	out := append([]string(nil), cLines...)
	for idx := len(starts) - 1; idx >= 0; idx-- {
		i := starts[idx]
		fileIndent := leadingWS(out[i])
		oldIndent := leadingWS(oLines[0])
		repl := strings.Split(reindentBlock(newNorm, oldIndent, fileIndent), "\n")
		out = append(out[:i], append(repl, out[i+k:]...)...)
		if !replaceAll {
			break
		}
	}
	joined := strings.Join(out, "\n")
	if useCRLF {
		joined = strings.ReplaceAll(joined, "\n", "\r\n")
	}
	return joined, len(starts), 2, nil
}

// fuzzyEdit performs a similarity-based single-best-window replacement. It only
// commits when the best window scores >= fuzzyAccept AND beats the next-best
// non-overlapping window by >= fuzzyMargin; otherwise it refuses and returns the
// closest region so the model can copy the exact current text.
func fuzzyEdit(cLines, oLines []string, newNorm string, useCRLF bool) (string, int, int, error) {
	k := len(oLines)
	bestScore, bestStart := -1.0, -1
	for i := 0; i+k <= len(cLines); i++ {
		if s := windowScore(cLines, oLines, i); s > bestScore {
			bestScore, bestStart = s, i
		}
	}
	if bestStart < 0 {
		return "", 0, 3, fmt.Errorf("old_string not found")
	}
	secondScore := -1.0
	for i := 0; i+k <= len(cLines); i++ {
		if absInt(i-bestStart) < k {
			continue // overlaps the best window
		}
		if s := windowScore(cLines, oLines, i); s > secondScore {
			secondScore = s
		}
	}
	if bestScore < fuzzyAccept || (secondScore >= 0 && bestScore-secondScore < fuzzyMargin) {
		region := strings.Join(cLines[bestStart:bestStart+k], "\n")
		return "", 0, 3, fmt.Errorf("no confident match for old_string (best similarity %.2f at lines %d-%d). Closest current text:\n%s\nCopy it exactly (whitespace included) and retry", bestScore, bestStart+1, bestStart+k, region)
	}

	out := append([]string(nil), cLines...)
	fileIndent := leadingWS(out[bestStart])
	oldIndent := leadingWS(oLines[0])
	repl := strings.Split(reindentBlock(newNorm, oldIndent, fileIndent), "\n")
	out = append(out[:bestStart], append(repl, out[bestStart+k:]...)...)
	joined := strings.Join(out, "\n")
	if useCRLF {
		joined = strings.ReplaceAll(joined, "\n", "\r\n")
	}
	return joined, 1, 3, nil
}

// windowScore is the mean per-line similarity of the k-line window of cLines
// starting at start against oLines.
func windowScore(cLines, oLines []string, start int) float64 {
	sum := 0.0
	for j := range oLines {
		sum += lineSimilarity(cLines[start+j], oLines[j])
	}
	return sum / float64(len(oLines))
}

// lineSimilarity is 1 - normalized Levenshtein distance over trimmed lines.
func lineSimilarity(a, b string) float64 {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return 1
	}
	maxLen := max(len(a), len(b))
	if maxLen == 0 {
		return 1
	}
	return 1 - float64(levenshtein(a, b))/float64(maxLen)
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func EditFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "edit_file",
			Description: "Replace a text snippet inside a file. Matching is exact first; if that fails it falls back to whitespace/indentation-tolerant line matching. By default old_string must resolve to exactly one location (safety). Set replace_all to true to substitute every occurrence. Use this for incremental edits instead of rewriting the whole file with write_file.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":        {Type: "string", Description: "Path to the file."},
					"old_string":  {Type: "string", Description: "Text currently in the file. Optional if start_line/end_line are specified. Exact whitespace is preferred but indentation differences are tolerated."},
					"new_string":  {Type: "string", Description: "Replacement text."},
					"replace_all": {Type: "boolean", Description: "Replace every occurrence (only applies when using old_string). Default false."},
					"start_line":  {Type: "number", Description: "Optional first line to replace (1-indexed, inclusive). Use instead of old_string for precise edits."},
					"end_line":    {Type: "number", Description: "Optional last line to replace (1-indexed, inclusive). Use instead of old_string for precise edits."},
				},
				Required: []string{"path", "new_string"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
				StartLine  int    `json:"start_line"`
				EndLine    int    `json:"end_line"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}

			var updated string
			var count int
			var tier int

			if a.StartLine != 0 || a.EndLine != 0 {
				if a.StartLine < 1 || a.EndLine < 1 {
					return "", fmt.Errorf("start_line and end_line must be 1-indexed (got start_line=%d, end_line=%d)", a.StartLine, a.EndLine)
				}
				lines := strings.Split(string(data), "\n")
				if a.StartLine > len(lines) || a.EndLine > len(lines) {
					return "", fmt.Errorf("start_line %d or end_line %d exceeds file length %d", a.StartLine, a.EndLine, len(lines))
				}
				if a.StartLine > a.EndLine {
					return "", fmt.Errorf("start_line %d is greater than end_line %d", a.StartLine, a.EndLine)
				}
				var newLines []string
				newLines = append(newLines, lines[:a.StartLine-1]...)
				newLines = append(newLines, a.NewString)
				newLines = append(newLines, lines[a.EndLine:]...)
				updated = strings.Join(newLines, "\n")
				count = 1
				tier = 1
			} else {
				if a.OldString == "" {
					return "", fmt.Errorf("old_string is required when start_line/end_line are not specified (use write_file to replace or write new content)")
				}
				var editErr error
				updated, count, tier, editErr = applyEdit(string(data), a.OldString, a.NewString, a.ReplaceAll)
				if editErr != nil {
					if tier >= 2 {
						return "", fmt.Errorf("%w in %s", editErr, a.Path)
					}
					return "", fmt.Errorf("%s: %w. If matching fails, consider reading the file with line numbers and using start_line/end_line for precise editing.", a.Path, editErr)
				}
			}

			// Verify-before-write: if the file parsed cleanly before the edit,
			// reject (don't write) an edit that would break its syntax.
			if verifyBytes(a.Path, data) == nil {
				if verr := verifyBytes(a.Path, []byte(updated)); verr != nil {
					return "", fmt.Errorf("edit rejected: it would introduce a syntax error in %s: %v\nNo changes were written — fix start_line/end_line/new_string and retry", a.Path, verr)
				}
			}
			info, err := os.Stat(a.Path)
			mode := os.FileMode(0o644)
			if err == nil {
				mode = info.Mode().Perm()
			}
			if err := os.WriteFile(a.Path, []byte(updated), mode); err != nil {
				return "", err
			}
			hash, _ := calculateHash(a.Path)
			tierNote := ""
			if a.StartLine != 0 || a.EndLine != 0 {
				tierNote = fmt.Sprintf(" (replaced lines %d to %d)", a.StartLine, a.EndLine)
			} else {
				switch tier {
				case 2:
					tierNote = " (matched after whitespace-normalization; copy exact text next time for precision)"
				case 3:
					tierNote = " (matched by fuzzy similarity — verify the diff carefully)"
				}
			}
			result := fmt.Sprintf("edited %s: replaced %d occurrence(s)%s\nNew Hash: %s", a.Path, count, tierNote, hash)
			if diff := unifiedDiff(string(data), updated, a.Path); diff != "" {
				result += "\n" + diff
			}
			return result, nil
		},
	}
}

// Tier-3 fuzzy thresholds: a match must be both strong in absolute terms and an
// unambiguous winner over any other (non-overlapping) region.
const (
	fuzzyAccept = 0.85
	fuzzyMargin = 0.10
)
