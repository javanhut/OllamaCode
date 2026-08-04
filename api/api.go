package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/javanhut/ollama_code/tools"
)

type Message struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking,omitempty"` // reasoning stream from thinking-capable models; never sent back
	ToolName  string           `json:"tool_name,omitempty"`
	ToolCalls []tools.ToolCall `json:"tool_calls,omitempty"`
}

type ChatRequest struct {
	Model    string          `json:"model"`
	Messages []Message       `json:"messages"`
	Stream   bool            `json:"stream"` // Set to true for streaming
	Tools    []tools.Tool    `json:"tools,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
	Format   json.RawMessage `json:"format,omitempty"` // JSON-schema for constrained decoding
	Think    *bool           `json:"think,omitempty"`  // enable reasoning on thinking-capable models
}

type ChatResponse struct {
	Model      string  `json:"model"`
	CreatedAt  string  `json:"created_at"`
	Message    Message `json:"message"`
	Done       bool    `json:"done"`
	Total      int64   `json:"total_duration,omitempty"`
	PromptEval int     `json:"prompt_eval_count,omitempty"`
	EvalCount  int     `json:"eval_count,omitempty"`
}

type GenerateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type GenerateResponse struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

type ModelListResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

type VersionResponse struct {
	Version string `json:"version"`
}

type ShowModelRequest struct {
	Model string `json:"model"`
}

// ShowModelResponse is the subset of Ollama's /api/show payload we use to
// discover a model's true context length and capabilities.
type ShowModelResponse struct {
	Capabilities []string       `json:"capabilities"`
	ModelInfo    map[string]any `json:"model_info"`
	Details      struct {
		Family        string `json:"family"`
		ParameterSize string `json:"parameter_size"` // e.g. "12.4B", "756b", "1t"
	} `json:"details"`
}

// ContextLength scans model_info for the architecture-specific
// "<family>.context_length" key (e.g. "llama.context_length") and returns it,
// or 0 if not reported.
func (r *ShowModelResponse) ContextLength() int {
	for k, v := range r.ModelInfo {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case json.Number:
			i, _ := n.Int64()
			return int(i)
		}
	}
	return 0
}

// SupportsTools reports whether the model advertises native tool-calling.
func (r *ShowModelResponse) SupportsTools() bool {
	return slices.Contains(r.Capabilities, "tools")
}

// SupportsThinking reports whether the model advertises a reasoning stream.
func (r *ShowModelResponse) SupportsThinking() bool {
	return slices.Contains(r.Capabilities, "thinking")
}

// ParamsB returns the model's parameter count in billions, or 0 if unknown.
// Prefers the exact model_info count, falls back to parsing the human-readable
// details.parameter_size ("12.4B", "756b", "1t").
func (r *ShowModelResponse) ParamsB() float64 {
	if v, ok := r.ModelInfo["general.parameter_count"]; ok {
		switch n := v.(type) {
		case float64:
			return n / 1e9
		case json.Number:
			f, _ := n.Float64()
			return f / 1e9
		}
	}
	s := strings.TrimSpace(strings.ToLower(r.Details.ParameterSize))
	if s == "" {
		return 0
	}
	mult := 1.0
	switch s[len(s)-1] {
	case 't':
		mult = 1000
	case 'b':
		mult = 1
	case 'm':
		mult = 0.001
	default:
		return 0
	}
	f, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0
	}
	return f * mult
}

type EmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type EmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

type Endpoint struct {
	Path    string
	Method  string
	Version int
}

var ollamaCalls map[string]Endpoint = map[string]Endpoint{
	"getModels": {
		Path:   "/api/tags",
		Method: "GET",
	},
	"getVersion": {
		Path:   "/api/version",
		Method: "GET",
	},
	"generateResponse": {
		Path:   "/api/generate",
		Method: "POST",
	},
	"chatResponse": {
		Path:   "/api/chat",
		Method: "POST",
	},
	"showModelDetails": {
		Path:   "/api/show",
		Method: "POST",
	},
	"pullModel": {
		Path:   "/api/pull",
		Method: "POST",
	},
	"runningModels": {
		Path:   "/api/ps",
		Method: "GET",
	},
	"getInputEmbedings": {
		Path:   "/api/embed",
		Method: "POST",
	},
}

// Provider selects the wire format a host speaks. The zero value is Ollama's
// native /api/* endpoints; ProviderOpenAI routes chat through the
// /v1/chat/completions translation in openai.go.
const (
	ProviderOllama = "ollama"
	ProviderOpenAI = "openai"
	// ProviderCursor is not an endpoint at all — see cursor.go.
)

// OllamaHost is one LLM endpoint. Despite the name it fronts both wire formats
// — a rename would churn every call site for no behavioral gain — so a session
// can hold a local Ollama daemon and an OpenAI-compatible provider at once.
type OllamaHost struct {
	uri      string
	apiKey   string
	provider string
	// trustWorkspace opts into passing --trust to the Cursor agent. Off by
	// default: it marks the working directory as trusted in Cursor without
	// asking, which is the user's call to make, not a default to inherit.
	trustWorkspace bool
}

// SetTrustWorkspace opts a Cursor provider into --trust, without which a
// headless run aborts on the workspace-trust prompt. Ignored by other providers.
func (o *OllamaHost) SetTrustWorkspace(t bool) { o.trustWorkspace = t }

// SetProvider selects the wire format. Anything other than ProviderOpenAI means
// native Ollama.
func (o *OllamaHost) SetProvider(p string) {
	o.provider = strings.ToLower(strings.TrimSpace(p))
}

// IsOpenAI reports whether this host speaks the OpenAI-compatible API.
func (o OllamaHost) IsOpenAI() bool { return o.provider == ProviderOpenAI }

func generatePath(call string, host OllamaHost) string {
	callPath := ollamaCalls[call].Path
	urlPath := fmt.Sprintf("%s%s", host.uri, callPath)
	return urlPath
}

func (o *OllamaHost) SetURI(uri string) {
	o.uri = uri
}

func (o *OllamaHost) URL() string {
	return o.uri
}

// SetAPIKey sets the bearer token used to authenticate requests. It is required
// to reach Ollama Cloud (https://ollama.com) directly and is harmless for a
// local daemon, which ignores it — so the same client serves local and cloud
// models without a separate code path.
func (o *OllamaHost) SetAPIKey(key string) {
	o.apiKey = strings.TrimSpace(key)
}

// applyAuth attaches the Authorization header when an API key is configured.
func (o OllamaHost) applyAuth(req *http.Request) {
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
}

// get performs an authenticated GET so local and cloud hosts share one path.
func (o OllamaHost) get(urlPath string) (*http.Response, error) {
	req, err := http.NewRequest("GET", urlPath, nil)
	if err != nil {
		return nil, err
	}
	o.applyAuth(req)
	return http.DefaultClient.Do(req)
}

// post performs an authenticated POST so local and cloud hosts share one path.
func (o OllamaHost) post(urlPath, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest("POST", urlPath, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	o.applyAuth(req)
	return http.DefaultClient.Do(req)
}

func (o OllamaHost) GetOllamaVersion() (string, error) {
	urlPath := generatePath("getVersion", o)
	resp, err := o.get(urlPath)
	if err != nil {
		return "", fmt.Errorf("failed to do call due to error: %v", err)
	}
	defer resp.Body.Close()

	var versionResp VersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&versionResp); err != nil {
		return "", fmt.Errorf("failed to decode version response: %v", err)
	}

	return versionResp.Version, nil
}

func (o OllamaHost) ShowModel(model string) (*ShowModelResponse, error) {
	if o.IsCursor() || o.IsOpenAI() {
		// No /v1 equivalent: capabilities and context length are not
		// discoverable. Callers fall back to defaults.
		return nil, fmt.Errorf("model introspection is not available on this provider")
	}
	urlPath := generatePath("showModelDetails", o)
	jsonData, err := json.Marshal(ShowModelRequest{Model: model})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal show request: %v", err)
	}
	resp, err := o.post(urlPath, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("http request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	var showResp ShowModelResponse
	if err := json.NewDecoder(resp.Body).Decode(&showResp); err != nil {
		return nil, fmt.Errorf("failed to decode show response: %v", err)
	}
	return &showResp, nil
}

func (o OllamaHost) GetModelList() (*ModelListResponse, error) {
	if o.IsCursor() {
		return o.modelsCursor()
	}
	if o.IsOpenAI() {
		return o.modelsOpenAI()
	}
	urlPath := generatePath("getModels", o)
	resp, err := o.get(urlPath)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch models: %v", err)
	}
	defer resp.Body.Close()

	var list ModelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("failed to decode model list: %v", err)
	}

	return &list, nil
}

func (o OllamaHost) GenerateResponse(req GenerateRequest) (*GenerateResponse, error) {
	if o.IsCursor() {
		res, err := o.chatOnceCursor(context.Background(), ChatRequest{Model: req.Model, Messages: []Message{{Role: "user", Content: req.Prompt}}})
		if err != nil {
			return nil, err
		}
		return &GenerateResponse{Model: req.Model, Response: res.Message.Content, Done: true}, nil
	}
	if o.IsOpenAI() {
		return o.generateOpenAI(req)
	}
	req.Stream = false

	urlPath := generatePath("generateResponse", o)
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %v", err)
	}

	resp, err := o.post(urlPath, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("http request failed: %v", err)
	}
	defer resp.Body.Close()

	var genResp GenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&genResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %v", err)
	}

	return &genResp, nil
}

func (o OllamaHost) ContinuousChat(ctx context.Context, req ChatRequest) (<-chan ChatResponse, <-chan error) {
	if o.IsCursor() {
		return o.chatCursor(ctx, req)
	}
	if o.IsOpenAI() {
		return o.chatOpenAI(ctx, req)
	}
	req.Stream = true

	respChan := make(chan ChatResponse)
	errChan := make(chan error, 1)

	go func() {
		defer close(respChan)
		defer close(errChan)

		urlPath := generatePath("chatResponse", o)
		jsonData, err := json.Marshal(req)
		if err != nil {
			errChan <- fmt.Errorf("failed to marshal chat request: %v", err)
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", urlPath, bytes.NewBuffer(jsonData))
		if err != nil {
			errChan <- fmt.Errorf("failed to create http request: %v", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("User-Agent", "OllamaCode/1.0 (Chat)")
		o.applyAuth(httpReq)

		client := &http.Client{}
		resp, err := client.Do(httpReq)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				errChan <- fmt.Errorf("http request failed: %v", err)
				return
			}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errChan <- fmt.Errorf("unexpected status code: %d", resp.StatusCode)
			return
		}

		sawDone := false
		decoder := json.NewDecoder(resp.Body)

		for {
			select {
			case <-ctx.Done():
				return
			default:
				var chunk ChatResponse
				err := decoder.Decode(&chunk)
				if err != nil {
					if err == io.EOF {
						if !sawDone {
							errChan <- fmt.Errorf("stream ended unexpectedly (connection closed before final chunk)")
						}
						return
					}
					errChan <- fmt.Errorf("error decoding stream chunk: %v", err)
					return
				}

				respChan <- chunk
				sawDone = chunk.Done

				if chunk.Done {
					return
				}
			}
		}
	}()

	return respChan, errChan
}

// ChatOnce performs a single non-streaming chat completion. It's used for
// constrained-decoding escalation (req.Format set to a JSON schema) where we
// need one complete, schema-valid object rather than a token stream.
func (o OllamaHost) ChatOnce(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if o.IsCursor() {
		return o.chatOnceCursor(ctx, req)
	}
	if o.IsOpenAI() {
		return o.chatOnceOpenAI(ctx, req)
	}
	req.Stream = false
	urlPath := generatePath("chatResponse", o)
	jsonData, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("failed to marshal chat request: %v", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", urlPath, bytes.NewBuffer(jsonData))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("failed to create http request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	o.applyAuth(httpReq)
	resp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("http request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ChatResponse{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ChatResponse{}, fmt.Errorf("failed to decode response: %v", err)
	}
	return out, nil
}

// PullProgress is one streamed status update from /api/pull. During a layer
// download Total/Completed carry byte counts; other phases send only Status
// (e.g. "pulling manifest", "verifying sha256 digest", "success").
type PullProgress struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
	Error     string `json:"error,omitempty"`
}

type pullRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// PullModel streams download progress for a model from /api/pull. Closing both
// channels (or an error) signals completion. It carries the API key, so cloud
// models can be pulled through an authenticated host just like local ones.
func (o OllamaHost) PullModel(ctx context.Context, model string) (<-chan PullProgress, <-chan error) {
	progCh := make(chan PullProgress)
	errCh := make(chan error, 1)

	go func() {
		defer close(progCh)
		defer close(errCh)

		if o.IsCursor() || o.IsOpenAI() {
			errCh <- fmt.Errorf("pulling is not available on this provider; models there are served remotely")
			return
		}
		urlPath := generatePath("pullModel", o)
		jsonData, err := json.Marshal(pullRequest{Model: model, Stream: true})
		if err != nil {
			errCh <- fmt.Errorf("failed to marshal pull request: %v", err)
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, "POST", urlPath, bytes.NewBuffer(jsonData))
		if err != nil {
			errCh <- fmt.Errorf("failed to create http request: %v", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		o.applyAuth(httpReq)

		resp, err := (&http.Client{}).Do(httpReq)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				errCh <- fmt.Errorf("http request failed: %v", err)
				return
			}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			errCh <- fmt.Errorf("unexpected status code: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			return
		}

		decoder := json.NewDecoder(resp.Body)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				var p PullProgress
				if err := decoder.Decode(&p); err != nil {
					if err == io.EOF {
						return
					}
					errCh <- fmt.Errorf("error decoding pull stream: %v", err)
					return
				}
				if p.Error != "" {
					errCh <- fmt.Errorf("%s", p.Error)
					return
				}
				progCh <- p
			}
		}
	}()

	return progCh, errCh
}

func (o OllamaHost) Embed(model string, inputs []string) ([][]float32, error) {
	if o.IsCursor() || o.IsOpenAI() {
		return nil, fmt.Errorf("embeddings are not available on this provider; point the embed model at a local Ollama daemon")
	}
	urlPath := generatePath("getInputEmbedings", o)
	req := EmbedRequest{Model: model, Input: inputs}
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embed request: %v", err)
	}
	resp, err := o.post(urlPath, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("http request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(body))
	}
	var embedResp EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("failed to decode embed response: %v", err)
	}
	return embedResp.Embeddings, nil
}
