package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	bootstrapResamples = 10000
	bootstrapSeed      = 20260101
)

// loadReport reads a result file, returning a clean error (never a panic) for
// missing, empty, corrupt, or result-less files.
func loadReport(path string) (report, error) {
	var r report
	b, err := os.ReadFile(path)
	if err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return r, fmt.Errorf("%s: file is empty (the run probably crashed or was interrupted before writing results)", path)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("%s: not a valid eval result file: %w", path, err)
	}
	if len(r.Results) == 0 {
		return r, fmt.Errorf("%s: contains no per-trial results", path)
	}
	return r, nil
}

type taskDiff struct {
	Task string  `json:"task"`
	A    float64 `json:"a_pass_rate"`
	B    float64 `json:"b_pass_rate"`
	ANs  [2]int  `json:"a_successes_trials"`
	BNs  [2]int  `json:"b_successes_trials"`
	Diff float64 `json:"diff"`
}

type comparison struct {
	A             string     `json:"a"`
	B             string     `json:"b"`
	Tasks         []taskDiff `json:"tasks"`
	Improved      []string   `json:"improved"`
	Regressed     []string   `json:"regressed"`
	OnlyInA       []string   `json:"only_in_a,omitempty"`
	OnlyInB       []string   `json:"only_in_b,omitempty"`
	MeanDiff      float64    `json:"mean_diff"`
	CILower       float64    `json:"ci_lower"`
	CIUpper       float64    `json:"ci_upper"`
	Resamples     int        `json:"bootstrap_resamples"`
	Verdict       string     `json:"verdict"`
	Distinguished bool       `json:"distinguishable"`
	Warnings      []string   `json:"warnings,omitempty"`
}

// metaOf returns the run metadata, synthesizing what older files do carry.
func metaOf(r report) (runMeta, bool) {
	if r.Metadata != nil {
		return *r.Metadata, true
	}
	return runMeta{Model: r.Model}, false
}

func metaWarnings(an, bn string, a, b report) []string {
	ma, okA := metaOf(a)
	mb, okB := metaOf(b)
	var w []string
	if !okA {
		w = append(w, an+" has no reproducibility metadata (older harness); temperature, seed, num_ctx and fixture set cannot be verified")
	}
	if !okB {
		w = append(w, bn+" has no reproducibility metadata (older harness); temperature, seed, num_ctx and fixture set cannot be verified")
	}
	diff := func(name, x, y string) {
		if x != y {
			w = append(w, fmt.Sprintf("%s differs: %s=%s vs %s=%s", name, an, x, bn, y))
		}
	}
	if ma.Model != mb.Model {
		diff("model", ma.Model, mb.Model)
	}
	if okA && okB {
		diff("temperature", fmtF(ma.Temperature), fmtF(mb.Temperature))
		diff("seed", fmtI(ma.Seed), fmtI(mb.Seed))
		diff("toolset", toolsetOf(&ma), toolsetOf(&mb))
		diff("rag", fmt.Sprint(ma.RAG), fmt.Sprint(mb.RAG))
		diff("num_ctx", fmt.Sprint(ma.NumCtx), fmt.Sprint(mb.NumCtx))
		diff("fixture_hash", ma.FixtureHash, mb.FixtureHash)
		diff("harness_version", ma.HarnessVer, mb.HarnessVer)
		if ma.ModelDigest != "" && mb.ModelDigest != "" {
			diff("model_digest", ma.ModelDigest, mb.ModelDigest)
		}
	}
	return w
}

func fmtF(p *float64) string {
	if p == nil {
		return "default"
	}
	return fmt.Sprint(*p)
}

func fmtI(p *int64) string {
	if p == nil {
		return "unset"
	}
	return fmt.Sprint(*p)
}

// compareReports pairs the two runs by task and bootstraps the mean paired
// per-task pass-rate difference (B minus A) over tasks.
func compareReports(an, bn string, a, b report) comparison {
	c := comparison{A: an, B: bn, Resamples: bootstrapResamples}
	ra, rb := map[string]taskRate{}, map[string]taskRate{}
	var orderA []string
	for _, t := range taskRates(a.Results) {
		ra[t.Task] = t
		orderA = append(orderA, t.Task)
	}
	for _, t := range taskRates(b.Results) {
		rb[t.Task] = t
	}
	var diffs []float64
	for _, name := range orderA {
		ta := ra[name]
		tb, ok := rb[name]
		if !ok {
			c.OnlyInA = append(c.OnlyInA, name)
			continue
		}
		d := tb.rate() - ta.rate()
		c.Tasks = append(c.Tasks, taskDiff{Task: name, A: ta.rate(), B: tb.rate(),
			ANs: [2]int{ta.C, ta.N}, BNs: [2]int{tb.C, tb.N}, Diff: d})
		diffs = append(diffs, d)
		switch {
		case d > 1e-12:
			c.Improved = append(c.Improved, name)
		case d < -1e-12:
			c.Regressed = append(c.Regressed, name)
		}
	}
	for _, t := range taskRates(b.Results) {
		if _, ok := ra[t.Task]; !ok {
			c.OnlyInB = append(c.OnlyInB, t.Task)
		}
	}
	c.Warnings = metaWarnings(an, bn, a, b)
	if len(c.OnlyInA)+len(c.OnlyInB) > 0 {
		c.Warnings = append(c.Warnings, fmt.Sprintf("task sets differ; comparing the %d shared tasks only", len(c.Tasks)))
	}
	if len(diffs) == 0 {
		c.Verdict = "no tasks in common; nothing to compare"
		return c
	}
	c.MeanDiff = meanOf(diffs)
	c.CILower, c.CIUpper = bootstrapMeanCI(diffs, bootstrapResamples, bootstrapSeed)
	switch {
	case len(diffs) < 2:
		c.Verdict = "only one shared task; difference not distinguishable from noise at this sample size"
	case c.CILower > 0:
		c.Distinguished = true
		c.Verdict = "B is better than A: the 95% interval for the paired difference excludes 0"
	case c.CIUpper < 0:
		c.Distinguished = true
		c.Verdict = "B is worse than A: the 95% interval for the paired difference excludes 0"
	default:
		c.Verdict = "difference not distinguishable from noise at this sample size (95% interval includes 0)"
	}
	return c
}

func printComparison(w io.Writer, c comparison) {
	fmt.Fprintf(w, "Paired comparison (outcome pass rate, B - A)\n  A: %s\n  B: %s\n\n", c.A, c.B)
	for _, wn := range c.Warnings {
		fmt.Fprintf(w, "WARNING: %s\n", wn)
	}
	if len(c.Warnings) > 0 {
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  %-22s %10s %10s %9s\n", "task", "A", "B", "diff")
	for _, t := range c.Tasks {
		fmt.Fprintf(w, "  %-22s %4d/%-5d %4d/%-5d %+8.1f%%\n", t.Task, t.ANs[0], t.ANs[1], t.BNs[0], t.BNs[1], t.Diff*100)
	}
	fmt.Fprintf(w, "\nImproved (B > A): %s\nRegressed (B < A): %s\n", listOrNone(c.Improved), listOrNone(c.Regressed))
	if len(c.OnlyInA) > 0 {
		fmt.Fprintf(w, "Only in A: %s\n", strings.Join(c.OnlyInA, ", "))
	}
	if len(c.OnlyInB) > 0 {
		fmt.Fprintf(w, "Only in B: %s\n", strings.Join(c.OnlyInB, ", "))
	}
	if len(c.Tasks) > 0 {
		fmt.Fprintf(w, "\nMean paired difference: %+.1f points (95%% bootstrap CI %+.1f to %+.1f; %d resamples over %d tasks)\n",
			c.MeanDiff*100, c.CILower*100, c.CIUpper*100, c.Resamples, len(c.Tasks))
	}
	fmt.Fprintf(w, "Verdict: %s\n", c.Verdict)
}

func listOrNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// runCompare implements -compare a.json,b.json. Returns a process exit code.
func runCompare(arg string, jsonOut bool, stdout, stderr io.Writer) int {
	parts := strings.Split(arg, ",")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		fmt.Fprintln(stderr, "usage: -compare a.json,b.json")
		return 2
	}
	pa, pb := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	a, errA := loadReport(pa)
	b, errB := loadReport(pb)
	if err := errors.Join(errA, errB); err != nil {
		fmt.Fprintln(stderr, "cannot compare:\n"+err.Error())
		return 1
	}
	c := compareReports(pa, pb, a, b)
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(c)
	} else {
		printComparison(stdout, c)
	}
	return 0
}
