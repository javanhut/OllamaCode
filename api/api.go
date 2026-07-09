package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/javanhut/ollama_code/mcp"
)

type Message struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolName  string         `json:"tool_name,omitempty"`
	ToolCalls []mcp.ToolCall `json:"tool_calls,omitempty"`
}

type ChatRequest struct {
	Model    string          `json:"model"`
	Messages []Message       `json:"messages"`
	Stream   bool            `json:"stream"` // Set to true for streaming
	Tools    []mcp.Tool      `json:"tools,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
	Format   json.RawMessage `json:"format,omitempty"` // JSON-schema for constrained decoding
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
		Family string `json:"family"`
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

type OllamaHost struct {
	uri    string
	apiKey string
}

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
