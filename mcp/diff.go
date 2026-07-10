package mcp

import (
	"fmt"
	"strings"
)

type diffOp struct {
	kind byte // ' ' keep, '-' delete, '+' add
	line string
}

// lineDiffOps computes a line-level diff of a -> b via longest-common-subsequence.
func lineDiffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// unifiedDiff renders a compact unified-style diff of oldStr -> newStr, showing
// changed lines with up to 3 lines of surrounding context. Returns "" when
// nothing changed, and a short note when the inputs are too large to diff.
func unifiedDiff(oldStr, newStr, path string) string {
	a := strings.Split(oldStr, "\n")
	b := strings.Split(newStr, "\n")
	if len(a) > 5000 || len(b) > 5000 {
		return fmt.Sprintf("(diff omitted: file too large, %d -> %d lines)", len(a), len(b))
	}
	ops := lineDiffOps(a, b)

	const context = 3
	keep := make([]bool, len(ops))
	changed := false
	for i, op := range ops {
		if op.kind != ' ' {
			changed = true
			for k := i - context; k <= i+context; k++ {
				if k >= 0 && k < len(ops) {
					keep[k] = true
				}
			}
		}
	}
	if !changed {
		return ""
	}

	// 1-based line number each op occupies in the old and new files.
	oldNo := make([]int, len(ops))
	newNo := make([]int, len(ops))
	on, nn := 1, 1
	for i, op := range ops {
		oldNo[i], newNo[i] = on, nn
		if op.kind != '+' {
			on++
		}
		if op.kind != '-' {
			nn++
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", path, path)
	for i := 0; i < len(ops); i++ {
		if !keep[i] {
			continue
		}
		j := i
		for j < len(ops) && keep[j] {
			j++
		}
		oldCount, newCount := 0, 0
		for _, op := range ops[i:j] {
			if op.kind != '+' {
				oldCount++
			}
			if op.kind != '-' {
				newCount++
			}
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", oldNo[i], oldCount, newNo[i], newCount)
		for _, op := range ops[i:j] {
			sb.WriteByte(op.kind)
			sb.WriteString(op.line)
			sb.WriteByte('\n')
		}
		i = j - 1
	}
	out := strings.TrimRight(sb.String(), "\n")
	if len(out) > 6000 {
		out = out[:6000] + "\n... (diff truncated)"
	}
	return out
}
