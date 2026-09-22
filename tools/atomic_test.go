package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicKeepsSymlinkAndMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.sh")
	link := filepath.Join(dir, "link.sh")
	if err := os.WriteFile(target, []byte("old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unsupported:", err)
	}

	if err := WriteFileAtomic(link, []byte("new\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced by a regular file (err=%v)", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new\n" {
		t.Fatalf("target not rewritten: %q", b)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode lost: %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}
