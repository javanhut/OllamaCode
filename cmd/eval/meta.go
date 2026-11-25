package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/api"
)

// harnessVersion is bumped when scoring or fixture semantics change in a way
// that makes results from different versions not directly comparable.
const harnessVersion = "eval-2"

// runMeta pins everything needed to reproduce (or knowingly not reproduce) a
// run. Pointers/omitempty mark values that were not set; older result files
// have no metadata block at all.
type runMeta struct {
	Model         string   `json:"model"`
	ModelDigest   string   `json:"model_digest,omitempty"`
	Quantization  string   `json:"quantization,omitempty"`
	ParameterSize string   `json:"parameter_size,omitempty"`
	Family        string   `json:"family,omitempty"`
	Temperature   *float64 `json:"temperature"` // null = model/server default
	Seed          *int64   `json:"seed"`        // null = unseeded; trial t uses seed+t-1
	NumCtx        int      `json:"num_ctx"`     // 0 = server default
	Constrain     bool     `json:"constrain"`
	Verify        bool     `json:"verify"`
	Toolset       string   `json:"toolset,omitempty"` // "" in older files means full
	RAG           bool     `json:"rag"`
	EmbedModel    string   `json:"embed_model,omitempty"`
	MaxSteps      int      `json:"max_steps"`
	HarnessVer    string   `json:"harness_version"`
	VCSRevision   string   `json:"vcs_revision,omitempty"`
	VCSModified   bool     `json:"vcs_modified,omitempty"`
	FixtureHash   string   `json:"fixture_hash"`
	FixtureCount  int      `json:"fixture_count"`
	GoVersion     string   `json:"go_version"`
	OS            string   `json:"os"`
	Arch          string   `json:"arch"`
	StartedAt     string   `json:"started_at"`
}

// sampling carries the optional decoding knobs injected into every request.
type sampling struct {
	Temperature *float64
	Seed        *int64
	NumCtx      int
}

// fixtureHash fingerprints the fixture definitions that influence results:
// names, prompts, seeded files and tool expectations. Check functions are code
// and cannot be hashed, so a changed checker is not detected; bump
// harnessVersion for such changes.
func fixtureHash(tasks []task) string {
	h := sha256.New()
	for _, t := range tasks {
		fmt.Fprintf(h, "task:%s\nprompt:%s\n", t.Name, t.Prompt)
		files := make([]string, 0, len(t.Setup))
		for p := range t.Setup {
			files = append(files, p)
		}
		sort.Strings(files)
		for _, p := range files {
			fmt.Fprintf(h, "file:%s\n%s\n", p, t.Setup[p])
		}
		fmt.Fprintf(h, "tools:%q\n", [][]string{t.Tools.Required, t.Tools.Optional, t.Tools.Forbidden, t.Tools.Ordered, t.Tools.AnyOf})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func collectMeta(model, host string, tasks []task, s sampling, steps int, constrain, verify, selftest bool, started time.Time) *runMeta {
	m := &runMeta{
		Model: model, Temperature: s.Temperature, Seed: s.Seed, NumCtx: s.NumCtx,
		Constrain: constrain, Verify: verify, MaxSteps: steps,
		Toolset: evalToolset, RAG: evalRAG, HarnessVer: harnessVersion, FixtureHash: fixtureHash(tasks), FixtureCount: len(tasks),
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		StartedAt: started.UTC().Format(time.RFC3339),
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range bi.Settings {
			switch kv.Key {
			case "vcs.revision":
				m.VCSRevision = kv.Value
			case "vcs.modified":
				m.VCSModified = kv.Value == "true"
			}
		}
	}
	if evalRAG {
		m.EmbedModel = evalEmbedModel
	}
	if !selftest {
		fillOllamaInfo(m, host)
	}
	return m
}

// fillOllamaInfo is best effort: any failure leaves the fields empty.
func fillOllamaInfo(m *runMeta, host string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	base := strings.TrimRight(host, "/")
	client := &http.Client{}

	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil); err == nil {
		if resp, err := client.Do(req); err == nil {
			var tags struct {
				Models []struct {
					Name   string `json:"name"`
					Model  string `json:"model"`
					Digest string `json:"digest"`
				} `json:"models"`
			}
			if json.NewDecoder(resp.Body).Decode(&tags) == nil {
				for _, t := range tags.Models {
					if t.Name == m.Model || t.Model == m.Model || t.Name == m.Model+":latest" {
						m.ModelDigest = t.Digest
						break
					}
				}
			}
			resp.Body.Close()
		}
	}
	body, _ := json.Marshal(map[string]string{"model": m.Model})
	if req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/show", bytes.NewReader(body)); err == nil {
		req.Header.Set("Content-Type", "application/json")
		if resp, err := client.Do(req); err == nil {
			var show struct {
				Details struct {
					Family            string `json:"family"`
					ParameterSize     string `json:"parameter_size"`
					QuantizationLevel string `json:"quantization_level"`
				} `json:"details"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&show) == nil {
				m.Quantization = show.Details.QuantizationLevel
				m.ParameterSize = show.Details.ParameterSize
				m.Family = show.Details.Family
			}
			resp.Body.Close()
		}
	}
}

// samplingClient wraps a chat client and injects temperature/seed into every
// request's options. The agent loop only exposes num_ctx, so the harness adds
// the rest here. Embedding keeps URL()/IsOpenAI() etc. visible to the agent's
// capability type assertions.
type samplingClient struct {
	api.OllamaHost
	temperature *float64
	seed        *int64 // already offset for this trial
}

func (c *samplingClient) ChatOnce(ctx context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	opts := make(map[string]any, len(req.Options)+2)
	for k, v := range req.Options {
		opts[k] = v
	}
	if c.temperature != nil {
		opts["temperature"] = *c.temperature
	}
	if c.seed != nil {
		opts["seed"] = *c.seed
	}
	req.Options = opts
	return c.OllamaHost.ChatOnce(ctx, req)
}

// toolsetOf normalizes the toolset of older files (no field) to "full".
func toolsetOf(m *runMeta) string {
	if m.Toolset == "" {
		return "full"
	}
	return m.Toolset
}
