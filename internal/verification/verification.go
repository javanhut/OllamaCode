package verification

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Plan struct {
	Command string
	Label   string
	Steps   []string
}

// Detect builds a conservative verification plan. It only derives commands
// from known project manifests and changed paths; arbitrary project scripts are
// never auto-discovered. An explicit override remains a user trust decision.
func Detect(root string, changed []string, override string) (Plan, bool) {
	if command := strings.TrimSpace(override); command != "" {
		return Plan{Command: command, Label: "verify", Steps: []string{command}}, true
	}
	if exists(filepath.Join(root, "go.mod")) {
		steps := goSteps(root, changed)
		steps = append(steps, "go build ./...")
		return Plan{Command: strings.Join(steps, " && "), Label: "targeted Go tests + build", Steps: steps}, true
	}
	if exists(filepath.Join(root, "Cargo.toml")) {
		steps := []string{"cargo test --no-run --quiet", "cargo check --quiet"}
		return Plan{Command: strings.Join(steps, " && "), Label: "cargo test --no-run + check", Steps: steps}, true
	}
	if exists(filepath.Join(root, "tsconfig.json")) {
		steps := []string{"npx --no-install tsc --noEmit"}
		return Plan{Command: steps[0], Label: "tsc --noEmit", Steps: steps}, true
	}
	return Plan{}, false
}

func goSteps(root string, changed []string) []string {
	packages := map[string]bool{}
	for _, path := range changed {
		if filepath.Ext(path) != ".go" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		rel, err := filepath.Rel(root, filepath.Dir(filepath.Clean(path)))
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		pkg := "."
		if rel != "." {
			pkg = "./" + filepath.ToSlash(rel)
		}
		packages[pkg] = true
	}
	ordered := make([]string, 0, len(packages))
	for pkg := range packages {
		ordered = append(ordered, pkg)
	}
	sort.Strings(ordered)
	steps := make([]string, 0, len(ordered))
	for _, pkg := range ordered {
		steps = append(steps, "go test "+pkg)
	}
	return steps
}

// Fingerprint identifies the exact changed-file state covered by verification.
func Fingerprint(root string, changed []string) string {
	paths := append([]string(nil), changed...)
	sort.Strings(paths)
	h := sha256.New()
	for _, path := range paths {
		clean := path
		if !filepath.IsAbs(clean) {
			clean = filepath.Join(root, clean)
		}
		fmt.Fprintf(h, "%s\x00", filepath.Clean(path))
		if data, err := os.ReadFile(clean); err == nil {
			h.Write(data)
		} else {
			fmt.Fprintf(h, "missing:%v", err)
		}
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
