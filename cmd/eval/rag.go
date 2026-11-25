package main

import (
	"fmt"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/semantic"
)

// Auto-RAG for the headless loop, mirroring tui/rag.go: embed the task, search
// a semantic index of the workspace, and inject a token-budgeted block. The TUI
// puts the block in its dynamic-context system message; here it is appended to
// the system prompt, which reaches the model in the same role. Tuning constants
// are the TUI's non-small-model defaults.
const (
	defaultEmbedModel = "nomic-embed-text"
	ragTopK           = 8
	ragMaxBlockTokens = 6000
	ragDefaultCtx     = 32768 // context assumed for the block budget when -num-ctx is unset
	evalCharsPerToken = 3.5
)

var (
	// Set once by main.
	evalToolset    = "full"
	evalRAG        = false
	evalEmbedModel = defaultEmbedModel
)

func approxTokens(s string) int {
	return int((float64(len(s)) + evalCharsPerToken - 1) / evalCharsPerToken)
}

// ragBlockFor builds an index of dir and returns the retrieval block for query.
// Any failure (embed model not pulled, host down) degrades to "" and an error
// the caller records, exactly as the TUI degrades silently.
func ragBlockFor(host api.OllamaHost, dir, query string, numCtx int) (string, error) {
	embedder := func(in []string) ([][]float32, error) { return host.Embed(evalEmbedModel, in) }
	idx, err := semantic.BuildIndex(dir, evalEmbedModel, embedder)
	if err != nil {
		return "", err
	}
	results, err := idx.Search(query, func(q string) ([]float32, error) {
		embs, err := host.Embed(evalEmbedModel, []string{q})
		if err != nil {
			return nil, err
		}
		if len(embs) == 0 {
			return nil, fmt.Errorf("empty embedding response")
		}
		return embs[0], nil
	}, ragTopK)
	if err != nil {
		return "", err
	}
	if numCtx <= 0 {
		numCtx = ragDefaultCtx
	}
	return formatRAGBlock(results, min(numCtx/8, ragMaxBlockTokens)), nil
}

// formatRAGBlock: see semantic.FormatBlock (shared with the TUI).
func formatRAGBlock(results []semantic.Result, maxTokens int) string {
	return semantic.FormatBlock(results, maxTokens, approxTokens)
}

// bashOnlyFilter exposes only run_shell. The loop ends on a plain-text answer,
// so no final-answer tool is needed. It replaces the fixture's own Filter.
func bashOnlyFilter(name string) bool { return name == "run_shell" }

// contractNAUnderBashOnly reports whether a fixture's tool contract demands
// tools other than run_shell, so it cannot be evaluated in bash-only mode.
func contractNAUnderBashOnly(e toolExpectation) bool {
	for _, f := range e.Forbidden {
		if f == "run_shell" || f == "*" {
			return true // the only tool on offer is forbidden by the contract
		}
	}
	for _, group := range [][]string{e.Required, e.Ordered, e.AnyOf} {
		for _, n := range group {
			if n != "run_shell" {
				return true
			}
		}
	}
	return false
}
