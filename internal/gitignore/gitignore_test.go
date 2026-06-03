package gitignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitignoreMatcher(t *testing.T) {
	tmp, err := os.MkdirTemp("", "gitignore-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	// Write a mock .gitignore
	gitIgnoreContent := `# Ignored files
*.log
/temp/
secrets.json
`
	if err := os.WriteFile(filepath.Join(tmp, ".gitignore"), []byte(gitIgnoreContent), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewMatcher(tmp)

	tests := []struct {
		path    string
		ignored bool
	}{
		// Default skip dirs
		{".git", true},
		{"node_modules", true},
		{"src/node_modules", true},
		{"vendor", true},

		// Mock gitignore matches
		{"test.log", true},
		{"src/test.log", true},
		{"temp/file.txt", true},
		{"secrets.json", true},

		// Allowed files
		{"main.go", false},
		{"src/main.go", false},
		{"temp_file.go", false},
	}

	for _, tt := range tests {
		res := m.IsIgnored(filepath.Join(tmp, tt.path))
		if res != tt.ignored {
			t.Errorf("IsIgnored(%q) = %v; want %v", tt.path, res, tt.ignored)
		}
	}
}
