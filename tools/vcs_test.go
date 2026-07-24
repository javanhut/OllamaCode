package tools

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestTranslateToIvaldi pins each git_* → ivaldi argv mapping to the real
// ivaldi command surface (verified via `ivaldi <sub> --help`). If a translator
// drifts from what ivaldi accepts, this fails. The gitArgv inputs mirror what
// the git.go handlers actually build.
func TestTranslateToIvaldi(t *testing.T) {
	// A real file so translateReset's os.Stat file-vs-ref check hits the file
	// branch deterministically.
	f := filepath.Join(t.TempDir(), "tracked.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		tool    string
		gitArgv []string
		want    []string
		wantOK  bool
	}{
		{"status", "git_status", []string{"status"}, []string{"status"}, true},
		{"log", "git_log", []string{"log", "-n", "10", "--graph", "--color=never", "--format=%h %ad %an: %s", "--date=short"}, []string{"log", "--oneline", "--limit", "10"}, true},
		{"log count", "git_log", []string{"log", "-n", "25"}, []string{"log", "--oneline", "--limit", "25"}, true},
		{"add", "git_add", []string{"add", "."}, []string{"gather", "."}, true},
		{"commit", "git_commit", []string{"commit", "-m", "msg"}, []string{"seal", "-m", "msg"}, true},
		{"commit amend", "git_commit", []string{"commit", "-m", "msg", "--amend"}, []string{"reseal", "-m", "msg"}, true},
		{"branch list", "git_branch", []string{"branch"}, []string{"timeline", "list"}, true},
		{"branch create", "git_branch", []string{"branch", "feature"}, []string{"timeline", "create", "feature"}, true},
		{"branch delete", "git_branch", []string{"branch", "-d", "feature"}, []string{"timeline", "remove", "feature"}, true},
		{"checkout switch", "git_checkout", []string{"checkout", "main"}, []string{"timeline", "switch", "main"}, true},
		{"checkout new", "git_checkout", []string{"checkout", "-b", "feature"}, []string{"timeline", "create", "feature"}, true},
		{"merge", "git_merge", []string{"merge", "feature"}, []string{"fuse", "feature"}, true},
		{"pull", "git_pull", []string{"pull", "--rebase", "origin"}, []string{"sync"}, true},
		{"push force", "git_push", []string{"push", "--force-with-lease", "origin"}, []string{"upload", "--force"}, true},
		{"push plain", "git_push", []string{"push", "origin"}, []string{"upload"}, true},
		{"stash unsupported", "git_stash", []string{"stash", "push"}, nil, false},
		{"diff unstaged", "git_diff", []string{"diff", "--color=never"}, []string{"diff"}, true},
		{"diff staged", "git_diff", []string{"diff", "--color=never", "--staged"}, []string{"diff", "--staged"}, true},
		{"diff two commits", "git_diff", []string{"diff", "--color=never", "main...feature"}, []string{"diff", "main", "feature"}, true},
		{"remote list", "git_remote", []string{"remote", "-v"}, []string{"portal", "list"}, true},
		{"remote add https", "git_remote", []string{"remote", "add", "origin", "https://github.com/owner/repo.git"}, []string{"portal", "add", "owner/repo"}, true},
		{"remote add scp", "git_remote", []string{"remote", "add", "origin", "git@github.com:owner/repo.git"}, []string{"portal", "add", "owner/repo"}, true},
		{"remote remove", "git_remote", []string{"remote", "remove", "owner/repo"}, []string{"portal", "remove", "owner/repo"}, true},
		// reset: unstage a file, unstage everything, and hard-reset to a ref.
		{"reset unstage file", "git_reset", []string{"reset", "HEAD", "--", f}, []string{"discard", f}, true},
		{"reset unstage all", "git_reset", []string{"reset", "HEAD"}, []string{"discard"}, true},
		{"reset hard ref", "git_reset", []string{"reset", "--hard", "abc123"}, []string{"rewind", "abc123", "--discard"}, true},
		{"reset soft ref", "git_reset", []string{"reset", "--soft", "abc123"}, []string{"rewind", "abc123"}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := translateToIvaldi(c.tool, c.gitArgv)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if c.wantOK && !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argv = %v, want %v", got, c.want)
			}
		})
	}
}

func TestGitURLToOwnerRepo(t *testing.T) {
	cases := map[string]string{
		"https://github.com/owner/repo.git": "owner/repo",
		"https://github.com/owner/repo":     "owner/repo",
		"git@github.com:owner/repo.git":     "owner/repo",
		"owner/repo":                        "owner/repo",
		"github.com/owner/repo":             "owner/repo",
	}
	for in, want := range cases {
		if got := gitURLToOwnerRepo(in); got != want {
			t.Errorf("gitURLToOwnerRepo(%q) = %q, want %q", in, got, want)
		}
	}
}
