package mcp

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

// verifyBytes performs a cheap, pure-Go syntax check on proposed file contents
// for the formats we can validate without external tools. Unknown extensions
// pass (returns nil). Used to gate edits: if a file parsed before an edit but
// not after, the edit is rejected rather than written.
func verifyBytes(path string, data []byte) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		fset := token.NewFileSet()
		if _, err := parser.ParseFile(fset, path, data, parser.SkipObjectResolution); err != nil {
			return err
		}
	case ".json":
		if !json.Valid(data) {
			return fmt.Errorf("invalid JSON")
		}
	}
	return nil
}
