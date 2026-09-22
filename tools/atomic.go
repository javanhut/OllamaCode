package tools

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path's contents so a reader, or a crash, sees the
// old bytes or the new ones, never a truncated mix. os.WriteFile truncates
// first, so an interrupted edit leaves a half-written source file.
//
// A symlink is followed and its target rewritten: renaming onto the link
// itself would replace it with a regular file. Hard links to the old inode are
// not preserved; that is the cost of the rename.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".ocode-*.tmp")
	if err != nil {
		// A directory we can write files in but not create files in is rare;
		// falling back beats failing the edit.
		return os.WriteFile(path, data, perm)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}
