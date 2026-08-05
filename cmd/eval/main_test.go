package main

import "testing"

func TestCheckToolContract(t *testing.T) {
	ok, _ := checkToolContract([]string{"read_file", "grep", "edit_file"}, toolExpectation{
		Required: []string{"read_file", "edit_file"}, Ordered: []string{"read_file", "edit_file"}, Forbidden: []string{"write_file"},
	})
	if !ok {
		t.Fatal("expected contract to pass")
	}
	ok, detail := checkToolContract([]string{"edit_file", "read_file"}, toolExpectation{Ordered: []string{"read_file", "edit_file"}})
	if ok || detail == "" {
		t.Fatal("expected order failure")
	}
	ok, _ = checkToolContract([]string{"read_file"}, toolExpectation{Forbidden: []string{"*"}})
	if ok {
		t.Fatal("expected wildcard prohibition to fail")
	}
}

func TestAccumulate(t *testing.T) {
	r := report{Results: []runResult{{Passed: true, ToolContract: true, DurationMS: 10}, {DurationMS: 30}}}
	accumulate(&r, []int64{10, 30})
	if r.PassRate != .5 || r.ToolContractRate != .5 || r.MeanDurationMS != 20 || r.P95DurationMS != 30 {
		t.Fatalf("unexpected report: %#v", r)
	}
}

func TestRunEvaluationRejectsUnknownTask(t *testing.T) {
	structured := true
	_, err := runEvaluation("unused", "http://localhost:11434", 1, 1, false, nil, &structured, "missing")
	if err == nil {
		t.Fatal("expected unknown task to fail before contacting the provider")
	}
}
