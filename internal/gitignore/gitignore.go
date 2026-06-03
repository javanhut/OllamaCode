package gitignore

import (
	"os"
	"path/filepath"

	ignore "github.com/sabhiram/go-gitignore"
)

// DefaultSkipDirs are directories that should always be ignored to prevent
// token blowups, even if not explicitly listed in .gitignore.
var DefaultSkipDirs = map[string]bool{
	".git":         true,
	".svn":         true,
	".hg":          true,
	".bzr":         true,
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	"dist":         true,
	"build":        true,
	"out":          true,
	"bin":          true,
	"obj":          true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".idea":        true,
	".vscode":      true,
	".next":        true,
	".nuxt":        true,
	"coverage":     true,
	".cache":       true,
	".terraform":   true,
}

type Matcher struct {
	gitIgnore *ignore.GitIgnore
	root      string
}

// NewMatcher creates a new gitignore matcher for the given root directory.
// It will load the .gitignore file if it exists.
func NewMatcher(root string) *Matcher {
	m := &Matcher{root: root}
	giPath := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(giPath); err == nil {
		if gi, err := ignore.CompileIgnoreFile(giPath); err == nil {
			m.gitIgnore = gi
		}
	}
	return m
}

// IsIgnored returns true if the path matches default skip directories or
// the parsed .gitignore rules.
func (m *Matcher) IsIgnored(path string) bool {
	name := filepath.Base(path)
	if DefaultSkipDirs[name] {
		return true
	}

	if m.gitIgnore == nil {
		return false
	}

	rel := path
	if filepath.IsAbs(path) {
		r, err := filepath.Rel(m.root, path)
		if err == nil {
			rel = r
		}
	}
	return m.gitIgnore.MatchesPath(rel)
}
