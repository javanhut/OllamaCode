//go:build treesitter

// Package tslang holds the tree-sitter grammar registry shared by the
// code-intelligence tools (package tools) and symbol-aware semantic chunking
// (internal/semantic). It compiles only with the `treesitter` build tag
// (make build-ts); the default CGO-free build compiles tslang_fallback.go
// instead, keeping the grammar C sources out of the default binary.
package tslang

import (
	"strings"
	"sync"
	"unsafe"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Mu serializes all parser, tree, and node access process-wide: tree-sitter
// parsers are not thread-safe, the node accessors read C memory, and both the
// tool scanners and the semantic chunker can run on separate goroutines. One
// mutex for parsers + scans keeps the concurrency story boring; scans take
// milliseconds and are rare next to model latency, so contention is a
// non-issue.
var Mu sync.Mutex

// langSpec describes one grammar before compilation. defQueries holds one
// pattern per definition form (they are compiled independently so a pattern
// the grammar rejects — renamed nodes across grammar versions — is dropped
// without taking the rest down). identKinds lists the leaf node kinds that
// count as references to a symbol.
type langSpec struct {
	language   unsafe.Pointer
	defQueries []string
	identKinds map[string]bool
}

// Language is a compiled grammar ready for scans.
type Language struct {
	Parser     *tree_sitter.Parser
	DefQueries []*tree_sitter.Query
	IdentKinds map[string]bool
}

// Definition patterns, one per node form. @name is the symbol identifier,
// @def the whole definition node (its start row is what gets reported, and
// doc comments attach above it). Both captures are required for a match to
// count; patterns that fail to compile against the shipped grammar version
// are skipped at init.
var langSpecs = map[string]langSpec{
	".go": {
		language: tree_sitter_go.Language(),
		defQueries: []string{
			`(function_declaration name: (identifier) @name) @def`,
			`(method_declaration name: (field_identifier) @name) @def`,
			`(type_spec name: (type_identifier) @name) @def`,
			`(var_spec name: (identifier) @name) @def`,
			`(const_spec name: (identifier) @name) @def`,
			`(short_var_declaration left: (expression_list (identifier) @name)) @def`,
		},
		identKinds: map[string]bool{
			"identifier": true, "field_identifier": true,
			"type_identifier": true, "package_identifier": true,
		},
	},
	".py": {
		language: tree_sitter_python.Language(),
		defQueries: []string{
			`(function_definition name: (identifier) @name) @def`,
			`(class_definition name: (identifier) @name) @def`,
			`(assignment left: (identifier) @name) @def`,
		},
		identKinds: map[string]bool{"identifier": true},
	},
	".rs": {
		language: tree_sitter_rust.Language(),
		defQueries: []string{
			`(function_item name: (identifier) @name) @def`,
			`(struct_item name: (type_identifier) @name) @def`,
			`(enum_item name: (type_identifier) @name) @def`,
			`(trait_item name: (type_identifier) @name) @def`,
			`(impl_item type: (type_identifier) @name) @def`,
			`(type_item name: (type_identifier) @name) @def`,
			`(const_item name: (identifier) @name) @def`,
			`(static_item name: (identifier) @name) @def`,
			`(mod_item name: (identifier) @name) @def`,
		},
		identKinds: map[string]bool{
			"identifier": true, "type_identifier": true, "field_identifier": true,
		},
	},
	".js": {
		language: tree_sitter_javascript.Language(),
		defQueries: []string{
			`(function_declaration name: (identifier) @name) @def`,
			`(generator_function_declaration name: (identifier) @name) @def`,
			`(class_declaration name: (identifier) @name) @def`,
			`(method_definition name: (property_identifier) @name) @def`,
			`(variable_declarator name: (identifier) @name) @def`,
		},
		identKinds: map[string]bool{
			"identifier": true, "property_identifier": true,
			"shorthand_property_identifier": true,
		},
	},
	".ts": {
		language: tree_sitter_typescript.LanguageTypescript(),
		defQueries: []string{
			`(function_declaration name: (identifier) @name) @def`,
			`(class_declaration name: (type_identifier) @name) @def`,
			`(method_definition name: (property_identifier) @name) @def`,
			`(interface_declaration name: (type_identifier) @name) @def`,
			`(type_alias_declaration name: (type_identifier) @name) @def`,
			`(enum_declaration name: (identifier) @name) @def`,
			`(variable_declarator name: (identifier) @name) @def`,
		},
		identKinds: map[string]bool{
			"identifier": true, "property_identifier": true,
			"type_identifier": true, "shorthand_property_identifier": true,
		},
	},
	".tsx": {
		language: tree_sitter_typescript.LanguageTSX(),
		defQueries: []string{
			`(function_declaration name: (identifier) @name) @def`,
			`(class_declaration name: (type_identifier) @name) @def`,
			`(method_definition name: (property_identifier) @name) @def`,
			`(interface_declaration name: (type_identifier) @name) @def`,
			`(type_alias_declaration name: (type_identifier) @name) @def`,
			`(enum_declaration name: (identifier) @name) @def`,
			`(variable_declarator name: (identifier) @name) @def`,
		},
		identKinds: map[string]bool{
			"identifier": true, "property_identifier": true,
			"type_identifier": true, "shorthand_property_identifier": true,
		},
	},
}

// Extra extensions sharing a grammar above: ".pyi" behaves like ".py",
// ".jsx"/".mjs"/".cjs" like ".js", ".mts"/".cts" like ".ts".
var extAliases = map[string]string{
	".pyi": ".py",
	".jsx": ".js", ".mjs": ".js", ".cjs": ".js",
	".mts": ".ts", ".cts": ".ts",
}

// langs is the compiled registry, keyed by lowercase file extension, dot
// included.
var langs = map[string]*Language{}

// init compiles the grammars and queries once. A language whose parser
// rejects the grammar (ABI mismatch) or whose definition patterns all fail to
// compile is left out of langs, which transparently routes its files to the
// callers' per-file fallbacks.
func init() {
	for ext, spec := range langSpecs {
		lang := tree_sitter.NewLanguage(spec.language)
		p := tree_sitter.NewParser()
		if err := p.SetLanguage(lang); err != nil {
			continue
		}
		tl := &Language{Parser: p, IdentKinds: spec.identKinds}
		for _, pat := range spec.defQueries {
			q, qerr := tree_sitter.NewQuery(lang, pat)
			if qerr != nil {
				continue
			}
			tl.DefQueries = append(tl.DefQueries, q)
		}
		if len(tl.DefQueries) == 0 {
			continue
		}
		langs[ext] = tl
	}
	for alias, canonical := range extAliases {
		if tl, ok := langs[canonical]; ok {
			langs[alias] = tl
		}
	}
}

// ForExt resolves the compiled language for a file extension, following
// aliases.
func ForExt(ext string) (*Language, bool) {
	tl, ok := langs[strings.ToLower(ext)]
	return tl, ok
}
