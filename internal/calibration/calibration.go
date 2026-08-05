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
	return result, nil
}

func calibrationRegistry() *tools.Registry {
	r := tools.NewRegistry()
	for _, definition := range []struct{ name, arg string }{{"inspect_file", "path"}, {"web_lookup", "query"}} {
		definition := definition
		r.Register(tools.Tool{Function: tools.Function{Name: definition.name, Description: "Calibration tool.", Parameters: tools.Schema{
			Type: "object", Properties: map[string]tools.Property{definition.arg: {Type: "string"}}, Required: []string{definition.arg},
		}}, Handler: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }})
	}
	return r
}

func CacheKey(model, provider, runtime, digest string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s", SuiteVersion, model, provider, runtime, digest)))
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
