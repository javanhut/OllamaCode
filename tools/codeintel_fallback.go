//go:build !treesitter

package tools

import "context"

// Default (CGO-free) build: the tree-sitter precision path is compiled out,
// so these stubs decline every request and codetools.go runs its historical
// grep/regex implementation unchanged. See codeintel_ts.go for the precise
// path, enabled with `go build -tags treesitter` / `make build-ts`.

func tsFindDefinitions(ctx context.Context, sym, defPat string) (string, bool) {
	return "", false
}

func tsFindSymbolLines(ctx context.Context, symbol, fileTypes, defPat string) (string, bool) {
	return "", false
}

func tsFindReferences(ctx context.Context, sym string) (string, bool) {
	return "", false
}

// Without grammars compiled in there is no parse, hence no parse errors.
func tsParseErrors(path string) []string {
	return nil
}
