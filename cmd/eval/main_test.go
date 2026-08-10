package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/javanhut/ollama_code/api"
)

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

func TestAccumulateByTask(t *testing.T) {
	r := report{Results: []runResult{
		{Task: "a", Trial: 1, Passed: true, BehaviorPassed: true, ToolContract: true, DurationMS: 10},
		{Task: "a", Trial: 2, ToolContract: true, DurationMS: 30},
		{Task: "b", Trial: 1, Passed: true, BehaviorPassed: true, ToolContract: true, DurationMS: 20},
	}}
	accumulate(&r, []int64{10, 30, 20})
	if len(r.ByTask) != 2 {
		t.Fatalf("expected 2 task summaries, got %#v", r.ByTask)
	}
	a, b := r.ByTask[0], r.ByTask[1]
	if a.Task != "a" || a.Trials != 2 || a.Passed != 1 || a.PassRate != .5 || a.ToolContractRate != 1 || a.MeanDurationMS != 20 {
		t.Fatalf("unexpected summary for a: %#v", a)
	}
	if b.Task != "b" || b.Trials != 1 || b.PassRate != 1 {
		t.Fatalf("unexpected summary for b: %#v", b)
	}
}

func TestResolveTrials(t *testing.T) {
	if got := resolveTrials(1, 3); got != 3 {
		t.Fatalf("-samples should override -runs, got %d", got)
	}
	if got := resolveTrials(2, 0); got != 2 {
		t.Fatalf("-runs should apply when -samples is unset, got %d", got)
	}
}

func TestRunEvaluationRejectsUnknownTask(t *testing.T) {
	structured := true
	_, err := runEvaluation("unused", "http://localhost:11434", 1, 1, false, nil, &structured, "missing", false, false)
	if err == nil {
		t.Fatal("expected unknown task to fail before contacting the provider")
	}
}

func TestScriptedClientPlaysStepsThenStops(t *testing.T) {
	c := &scriptedClient{steps: []scriptStep{
		{Calls: []scriptCall{{Name: "read_file", Args: `{"path":"x.txt"}`}}},
		{Content: "done"},
	}}
	resp, err := c.ChatOnce(context.Background(), api.ChatRequest{Tools: nil})
	if err != nil || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("unexpected first step: %#v err=%v", resp, err)
	}
	resp, _ = c.ChatOnce(context.Background(), api.ChatRequest{})
	if resp.Message.Content != "done" || len(resp.Message.ToolCalls) != 0 {
		t.Fatalf("unexpected final step: %#v", resp)
	}
	resp, _ = c.ChatOnce(context.Background(), api.ChatRequest{})
	if resp.Message.Content != "" || len(resp.Message.ToolCalls) != 0 {
		t.Fatalf("expected empty answer after script exhaustion, got %#v", resp)
	}
}

func TestScriptedClientAnswersRepairRequests(t *testing.T) {
	c := &scriptedClient{repair: `{"path":"f.txt","content":"x"}`}
	resp, err := c.ChatOnce(context.Background(), api.ChatRequest{Format: json.RawMessage(`{"type":"object"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(resp.Message.Content), &args); err != nil || args["path"] != "f.txt" {
		t.Fatalf("expected scripted repair args, got %q", resp.Message.Content)
	}
	c = &scriptedClient{}
	if _, err := c.ChatOnce(context.Background(), api.ChatRequest{Format: json.RawMessage(`{"type":"object"}`)}); err == nil {
		t.Fatal("expected an error when the fixture provides no repair args")
	}
}

func TestEveryFixtureHasScript(t *testing.T) {
	for _, task := range evalTasks() {
		if len(task.Script) == 0 {
			t.Errorf("fixture %q has no -selftest script", task.Name)
		}
	}
}

func TestSelftestEvaluationPasses(t *testing.T) {
	structured := true
	rep, err := runEvaluation("scripted-selftest", "", 15, 2, false, nil, &structured, "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * len(evalTasks()); rep.Total != want {
		t.Fatalf("expected %d results, got %d", want, rep.Total)
	}
	if rep.PassRate != 1 || rep.ToolContractRate != 1 {
		for _, r := range rep.Results {
			if !r.Passed {
				t.Logf("FAIL %s trial=%d: %s; %s (tools used: %v)", r.Task, r.Trial, r.Detail, r.ToolDetail, r.ToolsUsed)
			}
		}
		t.Fatalf("selftest must pass hermetically: pass=%v tools=%v", rep.PassRate, rep.ToolContractRate)
	}
	if len(rep.ByTask) != len(evalTasks()) {
		t.Fatalf("expected a summary per fixture, got %d", len(rep.ByTask))
	}
	for _, ts := range rep.ByTask {
		if ts.Trials != 2 || ts.PassRate != 1 {
			t.Fatalf("unexpected per-fixture aggregation: %#v", ts)
		}
	}
}

func TestSelftestExercisesArgumentRepair(t *testing.T) {
	structured := true
	rep, err := runEvaluation("scripted-selftest", "", 15, 1, false, nil, &structured, "repair-args", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 || !rep.Results[0].Passed {
		t.Fatalf("expected repair-args to pass, got %#v", rep.Results)
	}
	r := rep.Results[0]
	if r.ArgumentFailures != 1 || r.RepairAttempts != 1 || r.RepairsSucceeded != 1 {
		t.Fatalf("expected one failed argument repaired successfully, got %#v", r)
	}
}

func TestSelftestRejectsFixtureWithoutScript(t *testing.T) {
	structured := true
	tasks := evalTasks()
	for i := range tasks {
		if tasks[i].Name == "create-file" {
			tasks[i].Script = nil
		}
	}
	_, err := runTask(api.OllamaHost{}, "scripted-selftest", 15, 1, tasks[0], nil, &structured, false, nil, true)
	if err == nil {
		t.Fatal("expected -selftest to reject a fixture with no script")
	}
}
