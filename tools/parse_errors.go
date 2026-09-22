package tools

// maxParseErrors caps the syntax-error lines handed to the verification gate
// across all changed files; one broken file can otherwise drown the message.
const maxParseErrors = 20

// TreeSitterParseErrors returns "path:line: message" syntax-error diagnostics
// for the given files whose languages have a compiled tree-sitter grammar.
// In the default build (no `treesitter` tag) it always returns nil — callers
// must treat nil as "no signal", never as "no errors". Used by the verify
// gate for projects that have no manifest-driven build check, where a parse
// error is the only objective signal available.
func TreeSitterParseErrors(paths []string) []string {
	var out []string
	for _, path := range paths {
		out = append(out, tsParseErrors(path)...)
		if len(out) >= maxParseErrors {
			return out[:maxParseErrors]
		}
	}
	return out
}
