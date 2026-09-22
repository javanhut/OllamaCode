// Command finetune exports successful tool-call trajectories from redacted
// ocode traces as a JSONL instruction dataset for fine-tuning. See
// docs/training.md for the dataset schema and the LoRA recipe.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	tracepkg "github.com/javanhut/ollama_code/internal/trace"
)

// defaultTracePath mirrors the TUI's default trace destination
// (tui/config.go) so the exporter works with no flags on a used install.
func defaultTracePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "ollama_code-trace.jsonl")
	}
	return filepath.Join(dir, "ollama_code", "trace.jsonl")
}

func main() {
	tracePath := flag.String("trace", defaultTracePath(), "redacted trace file, or a directory of .jsonl traces")
	out := flag.String("out", "", "output JSONL path (default stdout)")
	minCalls := flag.Int("min-calls", 1, "minimum successful tool calls per exported record")
	onlyRated := flag.Bool("only-rated", false, "keep only turns rated good with /rate")
	flag.Parse()

	paths, err := traceFiles(*tracePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var records []tracepkg.DatasetRecord
	totals := tracepkg.ExportStats{Dropped: map[string]int{}}
	for _, path := range paths {
		recs, stats, err := tracepkg.Export(path, tracepkg.ExportOptions{MinCalls: *minCalls, OnlyRated: *onlyRated})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			continue
		}
		records = append(records, recs...)
		totals.Candidates += stats.Candidates
		totals.Kept += stats.Kept
		for reason, count := range stats.Dropped {
			totals.Dropped[reason] += count
		}
	}

	w := os.Stdout
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	fmt.Fprintf(os.Stderr, "%d trajectories: %d kept, %d dropped", totals.Candidates, totals.Kept, totals.Candidates-totals.Kept)
	reasons := make([]string, 0, len(totals.Dropped))
	for reason := range totals.Dropped {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		fmt.Fprintf(os.Stderr, " %s=%d", reason, totals.Dropped[reason])
	}
	fmt.Fprintln(os.Stderr)
}

// traceFiles resolves -trace to the list of trace files to read: the file
// itself, or every .jsonl file directly inside the directory.
func traceFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	matches, err := filepath.Glob(filepath.Join(path, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s contains no .jsonl traces", path)
	}
	return matches, nil
}
