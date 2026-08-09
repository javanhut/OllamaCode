//go:build !treesitter

// Package tslang is empty in the default CGO-free build: the tree-sitter
// grammar registry exists only with the `treesitter` build tag (see
// tslang.go). This file keeps the package importable so untagged builds of
// ./... do not fail on an empty package.
package tslang
