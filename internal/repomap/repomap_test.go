package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestBuildRanksReferencedFilesFirst(t *testing.T) {
	root := writeTree(t, map[string]string{
		"core/store.go":       "package core\n\ntype Store struct{}\n\nfunc OpenStore() *Store { return nil }\n",
		"a/a.go":              "package a\n\nfunc UseA() { core.OpenStore() }\n",
		"b/b.go":              "package b\n\nfunc UseB() { var s core.Store; _ = s; core.OpenStore() }\n",
		"c/c.go":              "package c\n\nfunc Lonely() {}\n",
		"web/app.ts":          "export function render() {}\nexport class Widget {}\nexport const handler = async (req) => {}\n",
		"tool.py":             "class Parser:\n    pass\n\nasync def run_parser():\n    pass\n",
		"node_modules/x/y.js": "function hidden() {}\n",
		".hidden/z.go":        "package z\nfunc Hidden() {}\n",
	})
	files, err := Build(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if files[0].Path != "core/store.go" {
		t.Fatalf("the file everything references must rank first, got %s", files[0].Path)
	}
	byPath := map[string]File{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	if _, ok := byPath["node_modules/x/y.js"]; ok {
		t.Error("vendored dependencies must be skipped")
	}
	if _, ok := byPath[".hidden/z.go"]; ok {
		t.Error("dot directories must be skipped")
	}
	names := func(p string) string {
		var n []string
		for _, d := range byPath[p].Defs {
			n = append(n, d.Name)
		}
		return strings.Join(n, ",")
	}
	if got := names("web/app.ts"); got != "render,Widget,handler" {
		t.Errorf("ts defs = %s", got)
	}
	if got := names("tool.py"); got != "Parser,run_parser" {
		t.Errorf("py defs = %s", got)
	}
	if got := names("core/store.go"); got != "Store,OpenStore" {
		t.Errorf("go defs = %s", got)
	}
}

func TestFocusBiasesRanking(t *testing.T) {
	root := writeTree(t, map[string]string{
		"hub.go":   "package x\nfunc Hub() {}\n",
		"u1.go":    "package x\nfunc U1() { Hub() }\n",
		"u2.go":    "package x\nfunc U2() { Hub() }\n",
		"leaf.go":  "package x\nfunc Leaf() {}\n",
		"focus.go": "package x\nfunc F() { Leaf() }\n",
	})
	files, _ := Build(root, Options{Focus: []string{"focus.go"}})
	rank := map[string]int{}
	for i, f := range files {
		rank[f.Path] = i
	}
	if rank["leaf.go"] > rank["hub.go"] {
		t.Fatalf("with focus on focus.go, its dependency leaf.go must outrank the global hub: %v", rank)
	}
}

func TestRenderRespectsBudget(t *testing.T) {
	var files []File
	for i := range 200 {
		files = append(files, File{Path: strings.Repeat("d/", 3) + "f" + string(rune('a'+i%26)) + ".go", Defs: []Def{{Name: "X", Line: 1, Text: "func X()"}}})
	}
	out := Render(files, 200)
	if len(out) > 200*4+300 {
		t.Fatalf("render exceeded its budget: %d bytes", len(out))
	}
	if !strings.Contains(out, "more files omitted") {
		t.Fatal("a truncated map must say so")
	}
	if Render(nil, 100) != "" {
		t.Fatal("no files, no map")
	}
}
