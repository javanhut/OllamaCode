package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeGofmt puts a stub `gofmt` on PATH with the given script body, so the
// format path can be exercised without depending on the real toolchain.
func fakeGofmt(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gofmt")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestFormatBytesUnknownExtension(t *testing.T) {
	fakeGofmt(t, "#!/bin/sh\necho formatted\n")
	in := []byte("some text\n")
	if got := formatBytes("notes.txt", in); string(got) != string(in) {
		t.Fatalf("unregistered extension should pass through, got %q", got)
	}
}

func TestFormatBytesNoBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on PATH
	in := []byte("package main\n")
	if got := formatBytes("main.go", in); string(got) != string(in) {
		t.Fatalf("missing formatter should pass through, got %q", got)
	}
}

func TestFormatBytesFormats(t *testing.T) {
	fakeGofmt(t, "#!/bin/sh\necho 'package main'\n")
	got := formatBytes("main.go", []byte("package  main\n"))
	if string(got) != "package main\n" {
		t.Fatalf("expected formatter output, got %q", got)
	}
}

func TestFormatBytesFailurePassesThrough(t *testing.T) {
	// A formatter that rejects the input (syntax error, bad flag) must leave the
	// caller's bytes alone rather than truncating the file to its partial output.
	fakeGofmt(t, "#!/bin/sh\necho 'partial'\nexit 2\n")
	in := []byte("package  main\n")
	if got := formatBytes("main.go", in); string(got) != string(in) {
		t.Fatalf("failed formatter should pass through, got %q", got)
	}
}

func TestFormatBytesEmptyOutputPassesThrough(t *testing.T) {
	fakeGofmt(t, "#!/bin/sh\nexit 0\n")
	in := []byte("package main\n")
	if got := formatBytes("main.go", in); string(got) != string(in) {
		t.Fatalf("empty formatter output should pass through, got %q", got)
	}
}

func TestFormatBytesDisabled(t *testing.T) {
	fakeGofmt(t, "#!/bin/sh\necho 'package main'\n")
	SetFormatEnabled(false)
	t.Cleanup(func() { SetFormatEnabled(true) })
	in := []byte("package  main\n")
	if got := formatBytes("main.go", in); string(got) != string(in) {
		t.Fatalf("disabled formatting should pass through, got %q", got)
	}
}
