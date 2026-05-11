package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/javanhut/ollama_code/mcp"
)

type Message struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolName  string         `json:"tool_name,omitempty"`
	ToolCalls []mcp.ToolCall `json:"tool_calls,omitempty"`
}

type ChatRequest struct {
	Model    string     `json:"model"`
	Messages []Message  `json:"messages"`
	Stream   bool       `json:"stream"` // Set to true for streaming
	Tools    []mcp.Tool `json:"tools,omitempty"`
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
	uri string
}

func generatePath(call string, host OllamaHost) string {
	callPath := ollamaCalls[call].Path
	urlPath := fmt.Sprintf("%s%s", host.uri, callPath)
	return urlPath
}

func (o *OllamaHost) SetURI(uri string) {
	o.uri = uri
}

func (o OllamaHost) GetOllamaVersion() (string, error) {
	urlPath := generatePath("getVersion", o)
	resp, err := http.Get(urlPath)
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

func (o OllamaHost) GetModelList() (*ModelListResponse, error) {
	urlPath := generatePath("getModels", o)
	resp, err := http.Get(urlPath)
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

	resp, err := http.Post(urlPath, "application/json", bytes.NewBuffer(jsonData))
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
						return
					}
					errChan <- fmt.Errorf("error decoding stream chunk: %v", err)
					return
				}

				respChan <- chunk

				if chunk.Done {
					return
				}
			}
		}
	}()

	return respChan, errChan
}
