package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// validateFixtures checks, without any model, that every fixture is
// well-formed and that its outcome check FAILS on the untouched starting
// workspace (a fixture that passes with no work is broken). It returns the
// problems found; an empty slice means all fixtures are valid.
//
// Limits of the fixture format: there is no reference-solution field other
// than Script (a scripted tool-call playback), which -selftest executes and
// grades; this mode does not run it. Checks that read the model's final answer
// are exercised here only with an empty answer.
func validateFixtures(tasks []task, w io.Writer) []string {
	var problems []string
	seen := map[string]bool{}
	for _, t := range tasks {
		var errs []string
		switch {
		case t.Name == "":
			errs = append(errs, "empty name")
		case seen[t.Name]:
			errs = append(errs, "duplicate name")
		}
		seen[t.Name] = true
		if strings.TrimSpace(t.Prompt) == "" {
			errs = append(errs, "empty prompt")
		}
		if t.Check == nil {
			errs = append(errs, "no Check (no outcome grading)")
		}
		if len(t.Script) == 0 {
			errs = append(errs, "no Script (cannot run under -selftest)")
		}
		for rel := range t.Setup {
			if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
				errs = append(errs, "setup path escapes workspace: "+rel)
			}
		}
		for _, req := range t.Tools.Required {
			for _, f := range t.Tools.Forbidden {
				if req == f || f == "*" {
					errs = append(errs, "tool required and forbidden: "+req)
				}
			}
		}
		if t.Check != nil {
			if ok, detail, err := checkOnStartingWorkspace(t); err != nil {
				errs = append(errs, "setup/check error: "+err.Error())
			} else if ok {
				errs = append(errs, "outcome check PASSES on the untouched workspace ("+detail+")")
			}
		}
		if len(errs) == 0 {
			fmt.Fprintf(w, "[OK]     %s\n", t.Name)
			continue
		}
		fmt.Fprintf(w, "[BROKEN] %s: %s\n", t.Name, strings.Join(errs, "; "))
		problems = append(problems, t.Name+": "+strings.Join(errs, "; "))
	}
	return problems
}

func checkOnStartingWorkspace(t task) (ok bool, detail string, err error) {
	dir, err := os.MkdirTemp("", "ollamacode-eval-validate-")
	if err != nil {
		return false, "", err
	}
	defer os.RemoveAll(dir)
	for rel, content := range t.Setup {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, "", err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return false, "", err
		}
	}
	defer func() {
		if r := recover(); r != nil {
			ok, detail, err = false, "", fmt.Errorf("check panicked: %v", r)
		}
	}()
	ok, detail = t.Check(dir, "")
	return ok, detail, nil
}
