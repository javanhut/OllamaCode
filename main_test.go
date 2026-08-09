package main

import (
	"io"
	"testing"
)

func TestParseFlagsHeadlessShort(t *testing.T) {
	f, err := parseFlags([]string{"-p", "say hi", "-json", "-max-steps", "5", "-model", "qwen3:8b"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if f.prompt != "say hi" || !f.json || f.maxSteps != 5 || f.model != "qwen3:8b" {
		t.Fatalf("unexpected flags: %+v", f)
	}
}

func TestParseFlagsHeadlessLong(t *testing.T) {
	// Go's flag package accepts --name for any flag; --prompt must land on the
	// same variable as -p.
	f, err := parseFlags([]string{"--prompt", "explain this repo"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if f.prompt != "explain this repo" {
		t.Fatalf("prompt = %q", f.prompt)
	}
	if f.json || f.maxSteps != 0 || f.model != "" {
		t.Fatalf("defaults changed: %+v", f)
	}
}

func TestParseFlagsNoArgsStartsTUI(t *testing.T) {
	f, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if f.prompt != "" {
		t.Fatalf("no args should leave prompt empty (TUI mode), got %q", f.prompt)
	}
}

func TestParseFlagsUnknownFlagFails(t *testing.T) {
	if _, err := parseFlags([]string{"-nope"}, io.Discard); err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
}
