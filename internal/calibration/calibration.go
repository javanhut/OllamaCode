package calibration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

const SuiteVersion = 1

type Client interface {
	ChatOnce(context.Context, api.ChatRequest) (api.ChatResponse, error)
}

type Result struct {
	SuiteVersion int       `json:"suite_version"`
	Model        string    `json:"model"`
	Provider     string    `json:"provider"`
	Runtime      string    `json:"runtime"`
	Digest       string    `json:"model_digest,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Runs         int       `json:"runs"`
	Correct      int       `json:"correct"`
	ValidArgs    int       `json:"valid_args"`
	Recommended  string    `json:"recommended_tier"`
	// CharsPerToken is the model's measured chars-per-token ratio, from
	// comparing known prompt lengths against prompt_eval_count. 0 means the
	// host reported no prompt_eval_count and callers keep their default
	// heuristic.
	CharsPerToken float64 `json:"chars_per_token,omitempty"`
}

func (r Result) Score() float64 {
	if r.Runs == 0 {
		return 0
	}
	return float64(r.Correct) / float64(r.Runs)
}

func Run(ctx context.Context, client Client, model, provider, runtime string) (Result, error) {
	result := Result{SuiteVersion: SuiteVersion, Model: model, Provider: provider, Runtime: runtime, CreatedAt: time.Now().UTC()}
	registry := calibrationRegistry()
	probes := []struct {
		prompt, expected string
		noTool           bool
	}{
		{"Call inspect_file with path main.go. Do not call another tool.", "inspect_file", false},
		{"Call web_lookup with query official documentation. Do not call another tool.", "web_lookup", false},
		{"Respond with ONLY 4. Do not call any tool. What is 2 + 2?", "", true},
	}
	for _, probe := range probes {
		requestCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		resp, err := client.ChatOnce(requestCtx, api.ChatRequest{Model: model,
			Messages: []api.Message{{Role: "system", Content: "Follow the user's tool instruction exactly."}, {Role: "user", Content: probe.prompt}},
			Tools:    registry.Definitions(), Options: map[string]any{"temperature": 0}})
		cancel()
		if err != nil {
			return result, err
		}
		result.Runs++
		calls := resp.Message.ToolCalls
		if len(calls) == 0 {
			calls = registry.ParseToolCallsFromContent(resp.Message.Content)
		}
		if probe.noTool {
			if len(calls) == 0 && strings.TrimSpace(resp.Message.Content) == "4" {
				result.Correct++
			}
			continue
		}
		if len(calls) == 1 && calls[0].Function.Name == probe.expected {
			result.Correct++
			if _, err := registry.Invoke(ctx, calls[0]); err == nil {
				result.ValidArgs++
			}
		}
	}
	if result.Correct == result.Runs && result.ValidArgs == 2 {
		result.Recommended = "strong"
	} else if result.Correct >= 2 {
		result.Recommended = "capable"
	} else {
		result.Recommended = "small"
	}
	if ratio, ok := measureCharsPerToken(ctx, client, model); ok {
		result.CharsPerToken = ratio
	}
	return result, nil
}

// ratioSamples are fixed texts of known length, sent without tools so the
// model's real chars-per-token ratio can be measured against prompt_eval_count.
// Prose and code are mixed because real prompts are both, and the samples are
// long enough that the chat template's constant token overhead is noise.
var ratioSamples = []string{
	strings.Repeat("The quick brown fox jumps over the lazy dog while the rain taps steadily against the windowpane. ", 12),
	strings.Repeat("func render(width int) string {\n\tif width <= 0 {\n\t\treturn \"\"\n\t}\n\treturn strings.Repeat(\"-\", width)\n}\n", 8),
}

// measureCharsPerToken estimates the model's chars-per-token ratio by sending
// the known-length ratioSamples and dividing by the summed prompt_eval_count.
// ok is false when the host errors or reports no prompt_eval_count (some
// OpenAI-compatible endpoints), leaving callers on their default heuristic.
func measureCharsPerToken(ctx context.Context, client Client, model string) (ratio float64, ok bool) {
	var chars, tokens int
	for _, sample := range ratioSamples {
		requestCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		resp, err := client.ChatOnce(requestCtx, api.ChatRequest{Model: model,
			Messages: []api.Message{{Role: "user", Content: sample}},
			Options:  map[string]any{"temperature": 0, "num_predict": 1}})
		cancel()
		if err != nil || resp.PromptEval <= 0 {
			return 0, false
		}
		chars += len(sample)
		tokens += resp.PromptEval
	}
	if chars == 0 || tokens == 0 {
		return 0, false
	}
	ratio = float64(chars) / float64(tokens)
	// Reject garbage counts from non-native backends: a real tokenizer lands
	// roughly between 1.5 and 8 chars per token for English prose and code.
	if ratio < 1.5 || ratio > 8 {
		return 0, false
	}
	return ratio, true
}

func calibrationRegistry() *tools.Registry {
	r := tools.NewRegistry()
	for _, definition := range []struct{ name, arg string }{{"inspect_file", "path"}, {"web_lookup", "query"}} {
		r.Register(tools.Tool{Function: tools.Function{Name: definition.name, Description: "Calibration tool.", Parameters: tools.Schema{
			Type: "object", Properties: map[string]tools.Property{definition.arg: {Type: "string"}}, Required: []string{definition.arg},
		}}, Handler: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }})
	}
	return r
}

func CacheKey(model, provider, runtime, digest string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d\x00%s\x00%s\x00%s\x00%s", SuiteVersion, model, provider, runtime, digest))
	return fmt.Sprintf("%x", sum[:12])
}

func cachePath(result Result) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ollama_code", "calibration", CacheKey(result.Model, result.Provider, result.Runtime, result.Digest)+".json"), nil
}

func Save(result Result) error {
	path, err := cachePath(result)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func Load(model, provider, runtime, digest string) (Result, error) {
	path, err := cachePath(Result{Model: model, Provider: provider, Runtime: runtime, Digest: digest})
	if err != nil {
		return Result{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return Result{}, err
	}
	if result.SuiteVersion != SuiteVersion {
		return Result{}, fmt.Errorf("calibration suite changed; run calibration again")
	}
	return result, nil
}
