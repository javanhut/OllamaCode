package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestWilsonKnownValues(t *testing.T) {
	// 16/21: Wilson 95% = z=1.96 closed form.
	iv := wilsonInterval(16, 21)
	if !near(iv.Point, 0.7619, 1e-3) || !near(iv.Lower, 0.54909, 1e-4) || !near(iv.Upper, 0.89372, 1e-4) {
		t.Fatalf("wilson 16/21 = %+v", iv)
	}
	// Boundary: 0/10 and 10/10 stay inside [0,1] and are not degenerate.
	z := wilsonInterval(0, 10)
	if z.Lower != 0 || !near(z.Upper, 0.2775, 2e-3) {
		t.Fatalf("wilson 0/10 = %+v", z)
	}
	o := wilsonInterval(10, 10)
	if o.Upper != 1 || !near(o.Lower, 0.7225, 2e-3) {
		t.Fatalf("wilson 10/10 = %+v", o)
	}
	if wilsonInterval(0, 0).Valid {
		t.Fatal("n=0 must be invalid")
	}
}

func TestClusteredInterval(t *testing.T) {
	// rates 1,1,0.5,0.5 -> mean .75, sd = sqrt(0.25/3*... ) computed by hand:
	// deviations .25,.25,-.25,-.25 -> ss=.25, var=.25/3, se=sqrt(var/4)=0.14434
	iv := clusteredInterval([]float64{1, 1, 0.5, 0.5})
	if !iv.Valid || !near(iv.Point, 0.75, 1e-12) || !near(iv.SE, 0.14434, 1e-4) {
		t.Fatalf("clustered = %+v", iv)
	}
	if !near(iv.Lower, 0.75-z95*0.14434, 1e-3) {
		t.Fatalf("lower = %v", iv.Lower)
	}
	if clusteredInterval([]float64{0.5}).Valid {
		t.Fatal("single task must be invalid")
	}
}

func TestPassKEstimators(t *testing.T) {
	// c=2 of n=3: pass^2 = C(2,2)/C(3,2)=1/3, pass^3 = 0, pass@2 = 1-C(1,2)/3 = 1.
	if !near(passHatK(2, 3, 2), 1.0/3, 1e-12) || passHatK(2, 3, 3) != 0 || passHatK(2, 3, 1) < 0.666 {
		t.Fatal("passHatK wrong")
	}
	if passAtK(2, 3, 2) != 1 {
		t.Fatal("passAtK(2,3,2)")
	}
	// c=1 of n=4, k=2: pass@2 = 1 - C(3,2)/C(4,2) = 1-3/6 = .5
	if !near(passAtK(1, 4, 2), 0.5, 1e-12) || !near(passAtK(1, 4, 1), 0.25, 1e-12) {
		t.Fatal("passAtK wrong")
	}
	if passHatK(3, 3, 3) != 1 || passAtK(0, 3, 3) != 0 {
		t.Fatal("extremes wrong")
	}
}

func mkResults(task string, outcomes ...bool) []runResult {
	var out []runResult
	for i, o := range outcomes {
		out = append(out, runResult{Task: task, Trial: i + 1, BehaviorPassed: o, Passed: o})
	}
	return out
}

func TestComputeStatsOutcomeBased(t *testing.T) {
	var rs []runResult
	rs = append(rs, mkResults("a", true, true, true)...)
	rs = append(rs, mkResults("b", true, false, false)...)
	// Tool contract failure must not affect the outcome headline.
	rs[0].ToolContract = false
	st := computeStats(rs)
	if st.Tasks != 2 || st.Trials != 6 || st.K != 3 || st.Successes != 4 {
		t.Fatalf("%+v", st)
	}
	if !near(st.Pass1, (1+1.0/3)/2, 1e-12) || !near(st.PassHatK, 0.5, 1e-12) || !near(st.PassAtK, 1, 1e-12) {
		t.Fatalf("%+v", st)
	}
	if len(st.Curve) != 3 {
		t.Fatalf("curve %v", st.Curve)
	}
}

func TestBootstrapDeterministic(t *testing.T) {
	xs := []float64{0, 0.33, 0.67, 1, 0, 0.33, 0.67}
	l1, h1 := bootstrapMeanCI(xs, 2000, 7)
	l2, h2 := bootstrapMeanCI(xs, 2000, 7)
	if l1 != l2 || h1 != h2 {
		t.Fatal("bootstrap not deterministic")
	}
	if !(l1 < meanOf(xs) && meanOf(xs) < h1) {
		t.Fatalf("mean outside CI: %v %v %v", l1, meanOf(xs), h1)
	}
	if l, h := bootstrapMeanCI([]float64{0.5, 0.5, 0.5}, 500, 1); l != 0.5 || h != 0.5 {
		t.Fatal("constant sample must give a point CI")
	}
}

func writeJSON(t *testing.T, dir, name string, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func synth(model string, meta *runMeta, rates map[string]int) report {
	r := report{Model: model, Metadata: meta}
	for _, task := range []string{"t1", "t2", "t3", "t4", "t5"} {
		var o []bool
		for i := range 3 {
			o = append(o, i < rates[task])
		}
		r.Results = append(r.Results, mkResults(task, o...)...)
	}
	return r
}

func TestCompareSynthetic(t *testing.T) {
	dir := t.TempDir()
	temp := 0.0
	meta := &runMeta{Model: "m1", Temperature: &temp, FixtureHash: "abc", HarnessVer: harnessVersion}
	a := synth("m1", meta, map[string]int{"t1": 3, "t2": 3, "t3": 2, "t4": 3, "t5": 3})
	// b: older file, no metadata, differs on t3 (better) and t4 (worse).
	b := synth("m2", nil, map[string]int{"t1": 3, "t2": 3, "t3": 3, "t4": 2, "t5": 3})
	pa := writeJSON(t, dir, "a.json", a)
	pb := writeJSON(t, dir, "b.json", b)
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(corrupt, []byte("{not json"), 0o644)

	var out, errb bytes.Buffer
	if code := runCompare(pa+","+pb, false, &out, &errb); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"no reproducibility metadata", "model differs", "Improved (B > A): t3", "Regressed (B < A): t4", "not distinguishable from noise"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	c := compareReports(pa, pb, a, b)
	if !near(c.MeanDiff, 0, 1e-9) || c.Distinguished {
		t.Fatalf("%+v", c)
	}
	c2 := compareReports(pa, pb, a, b)
	if c.CILower != c2.CILower || c.CIUpper != c2.CIUpper {
		t.Fatal("compare not deterministic")
	}

	for _, bad := range []string{empty, corrupt, filepath.Join(dir, "missing.json")} {
		out.Reset()
		errb.Reset()
		if code := runCompare(pa+","+bad, false, &out, &errb); code != 1 || errb.Len() == 0 {
			t.Fatalf("%s: code %d stderr %q", bad, code, errb.String())
		}
	}
	if code := runCompare("onlyone.json", false, &out, &errb); code != 2 {
		t.Fatalf("usage code %d", code)
	}
}

func TestCompareDetectsClearDifference(t *testing.T) {
	good := synth("g", nil, map[string]int{"t1": 3, "t2": 3, "t3": 3, "t4": 3, "t5": 3})
	bad := synth("b", nil, map[string]int{"t1": 0, "t2": 1, "t3": 0, "t4": 1, "t5": 0})
	c := compareReports("bad", "good", bad, good)
	if !c.Distinguished || c.CILower <= 0 || !strings.Contains(c.Verdict, "better") {
		t.Fatalf("%+v", c)
	}
}

func TestMetaWarningsMismatch(t *testing.T) {
	t0, t1 := 0.0, 0.7
	s1, s2 := int64(1), int64(2)
	a := report{Model: "m", Metadata: &runMeta{Model: "m", Temperature: &t0, Seed: &s1, FixtureHash: "x"}}
	b := report{Model: "m", Metadata: &runMeta{Model: "m", Temperature: &t1, Seed: &s2, FixtureHash: "y"}}
	w := strings.Join(metaWarnings("a", "b", a, b), "\n")
	for _, want := range []string{"temperature differs", "seed differs", "fixture_hash differs"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing %q in %s", want, w)
		}
	}
}

func TestFixtureHashStable(t *testing.T) {
	if fixtureHash(evalTasks()) != fixtureHash(evalTasks()) {
		t.Fatal("fixture hash must be deterministic")
	}
}

func TestValidateFixtures(t *testing.T) {
	var buf bytes.Buffer
	if problems := validateFixtures(evalTasks(), &buf); len(problems) != 0 {
		t.Fatalf("shipped fixtures invalid: %v\n%s", problems, buf.String())
	}
	broken := []task{{
		Name: "free-pass", Prompt: "do nothing",
		Check:  func(dir, out string) (bool, string) { return true, "always" },
		Script: []scriptStep{{Content: "done"}},
	}}
	buf.Reset()
	if problems := validateFixtures(broken, &buf); len(problems) != 1 || !strings.Contains(problems[0], "PASSES on the untouched") {
		t.Fatalf("expected free-pass flagged: %v", problems)
	}
}

func TestLowerBoundGate(t *testing.T) {
	st := computeStats(append(mkResults("a", true, true, true), mkResults("b", true, true, true)...))
	if st.lowerBound() >= 1 {
		t.Fatal("lower bound of a perfect small sample must be below 1")
	}
}

func TestBashOnlyContractNA(t *testing.T) {
	if !contractNAUnderBashOnly(toolExpectation{Required: []string{"read_file"}}) ||
		!contractNAUnderBashOnly(toolExpectation{Forbidden: []string{"run_shell"}}) ||
		!contractNAUnderBashOnly(toolExpectation{Forbidden: []string{"*"}}) {
		t.Fatal("expected n/a")
	}
	if contractNAUnderBashOnly(toolExpectation{Required: []string{"run_shell"}, Forbidden: []string{"write_file"}}) {
		t.Fatal("shell-only contract is applicable")
	}
	if !bashOnlyFilter("run_shell") || bashOnlyFilter("read_file") {
		t.Fatal("filter wrong")
	}
}

func TestAccumulateToolContractNA(t *testing.T) {
	r := report{Results: []runResult{
		{Task: "a", BehaviorPassed: true, ToolContract: true, ToolContractNA: true, Passed: true},
		{Task: "b", BehaviorPassed: true, ToolContract: true, Passed: true},
		{Task: "b", BehaviorPassed: true, ToolContract: false},
	}}
	accumulate(&r, nil)
	if r.ToolContractNA != 1 || r.ToolContracts != 1 || r.ToolContractRate != 0.5 {
		t.Fatalf("%+v", r)
	}
	all := report{Results: []runResult{{Task: "a", ToolContract: true, ToolContractNA: true}}}
	accumulate(&all, nil)
	if all.ToolContractRate != 1 {
		t.Fatal("all-n/a must not trip the gate")
	}
}

func TestCompareWarnsOnToolsetAndRAG(t *testing.T) {
	a := report{Model: "m", Metadata: &runMeta{Model: "m"}}
	b := report{Model: "m", Metadata: &runMeta{Model: "m", Toolset: "bash-only", RAG: true}}
	w := strings.Join(metaWarnings("a", "b", a, b), "\n")
	if !strings.Contains(w, "toolset differs: a=full vs b=bash-only") || !strings.Contains(w, "rag differs") {
		t.Fatal(w)
	}
}
