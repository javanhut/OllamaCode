package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/javanhut/ollama_code/mcp"
)

// MLXClient talks to the bundled MLX bridge (internal/backend), which serves an
// OpenAI-compatible API in front of mlx_lm (text) and mlx_vlm (vision). It
// implements api.Provider so the TUI can use it interchangeably with the Ollama
// client. Translation between OllamaCode's chat shapes and the OpenAI wire
// format happens here; the bridge itself stays a thin OpenAI server.
type MLXClient struct {
	uri    string
	client *http.Client
}

func (c *MLXClient) SetURI(uri string) {
	c.uri = strings.TrimRight(uri, "/")
}

func (c *MLXClient) httpClient() *http.Client {
	if c.client == nil {
		c.client = &http.Client{}
	}
	return c.client
}

// ----- OpenAI wire types (only the fields we use) -----

type oaiMessage struct {
	Role      string        `json:"role"`
	Content   string        `json:"content"`
	Name      string        `json:"name,omitempty"`
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Index    int    `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // OpenAI sends arguments as a JSON-encoded string
	} `json:"function"`
}

type oaiChatRequest struct {
	Model          string          `json:"model"`
	Messages       []oaiMessage    `json:"messages"`
	Stream         bool            `json:"stream"`
	Tools          []mcp.Tool      `json:"tools,omitempty"` // mcp.Tool already marshals to OpenAI's {type,function} shape
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	StreamOptions  *streamOptions  `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// non-streaming response
type oaiChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role      string        `json:"role"`
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage oaiUsage `json:"usage"`
}

// streaming chunk
type oaiStreamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaiUsage `json:"usage"`
}

type oaiModelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// ----- request translation -----

func toOAIMessages(in []Message) []oaiMessage {
	out := make([]oaiMessage, 0, len(in))
	for _, m := range in {
		om := oaiMessage{Role: m.Role, Content: m.Content}
		if m.Role == "tool" && m.ToolName != "" {
			om.Name = m.ToolName
		}
		for _, tc := range m.ToolCalls {
			var oc oaiToolCall
			oc.Type = "function"
			oc.Function.Name = tc.Function.Name
			oc.Function.Arguments = string(tc.Function.Arguments)
			om.ToolCalls = append(om.ToolCalls, oc)
		}
		out = append(out, om)
	}
	return out
}

func (c *MLXClient) buildRequest(req ChatRequest, stream bool) oaiChatRequest {
	o := oaiChatRequest{
		Model:    req.Model,
		Messages: toOAIMessages(req.Messages),
		Stream:   stream,
		Tools:    req.Tools,
	}
	if stream {
		o.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if v, ok := floatOption(req.Options, "temperature"); ok {
		o.Temperature = &v
	}
	if v, ok := floatOption(req.Options, "top_p"); ok {
		o.TopP = &v
	}
	if v, ok := intOption(req.Options, "num_predict"); ok && v > 0 {
		o.MaxTokens = &v
	}
	if len(req.Format) > 0 {
		// Ask for schema-guided JSON; servers that don't support json_schema
		// typically still return parseable content, so callers re-validate.
		o.ResponseFormat = json.RawMessage(fmt.Sprintf(
			`{"type":"json_schema","json_schema":{"name":"response","schema":%s}}`, string(req.Format)))
	}
	return o
}

func floatOption(opts map[string]any, key string) (float64, bool) {
	v, ok := opts[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func intOption(opts map[string]any, key string) (int, bool) {
	v, ok := opts[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func oaiToMCPCalls(calls []oaiToolCall) []mcp.ToolCall {
	out := make([]mcp.ToolCall, 0, len(calls))
	for _, tc := range calls {
		args := tc.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, mcp.ToolCall{
			Function: mcp.ToolCallFunction{
				Name:      tc.Function.Name,
				Arguments: json.RawMessage(args),
			},
		})
	}
	return out
}

// ----- Provider implementation -----

func (c *MLXClient) GetModelList() (*ModelListResponse, error) {
	resp, err := c.httpClient().Get(c.uri + "/v1/models")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch MLX models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	var ml oaiModelList
	if err := json.NewDecoder(resp.Body).Decode(&ml); err != nil {
		return nil, fmt.Errorf("failed to decode MLX model list: %v", err)
	}
	out := &ModelListResponse{}
	for _, d := range ml.Data {
		out.Models = append(out.Models, struct {
			Name string `json:"name"`
		}{Name: d.ID})
	}
	return out, nil
}

// ShowModel synthesizes a profile. The bridge has no per-model /api/show; we
// return an empty Capabilities list so the caller keeps its optimistic
// "tools supported" default and tool calls flow through the text-parse fallback.
func (c *MLXClient) ShowModel(model string) (*ShowModelResponse, error) {
	return &ShowModelResponse{}, nil
}

func (c *MLXClient) Embed(model string, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": model, "input": inputs})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embed request: %v", err)
	}
	resp, err := c.httpClient().Post(c.uri+"/v1/embeddings", "application/json", bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("http request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("failed to decode embed response: %v", err)
	}
	embs := make([][]float32, 0, len(out.Data))
	for _, d := range out.Data {
		embs = append(embs, d.Embedding)
	}
	return embs, nil
}

// GenerateResponse adapts a single-prompt completion onto the chat endpoint
// (the bridge has no separate /generate route).
func (c *MLXClient) GenerateResponse(req GenerateRequest) (*GenerateResponse, error) {
	out, err := c.ChatOnce(context.Background(), ChatRequest{
		Model:    req.Model,
		Messages: []Message{{Role: "user", Content: req.Prompt}},
	})
	if err != nil {
		return nil, err
	}
	return &GenerateResponse{Model: req.Model, Response: out.Message.Content, Done: true}, nil
}

func (c *MLXClient) ChatOnce(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body, err := json.Marshal(c.buildRequest(req, false))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("failed to marshal chat request: %v", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.uri+"/v1/chat/completions", bytes.NewBuffer(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("failed to create http request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("http request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return ChatResponse{}, fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(b))
	}
	var oresp oaiChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&oresp); err != nil {
		return ChatResponse{}, fmt.Errorf("failed to decode response: %v", err)
	}
	out := ChatResponse{
		Model:      oresp.Model,
		Done:       true,
		PromptEval: oresp.Usage.PromptTokens,
		EvalCount:  oresp.Usage.CompletionTokens,
		Message:    Message{Role: "assistant"},
	}
	if len(oresp.Choices) > 0 {
		ch := oresp.Choices[0]
		out.Message.Content = ch.Message.Content
		out.Message.ToolCalls = oaiToMCPCalls(ch.Message.ToolCalls)
	}
	return out, nil
}

func (c *MLXClient) ContinuousChat(ctx context.Context, req ChatRequest) (<-chan ChatResponse, <-chan error) {
	respChan := make(chan ChatResponse)
	errChan := make(chan error, 1)

	go func() {
		defer close(respChan)
		defer close(errChan)

		body, err := json.Marshal(c.buildRequest(req, true))
		if err != nil {
			errChan <- fmt.Errorf("failed to marshal chat request: %v", err)
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, "POST", c.uri+"/v1/chat/completions", bytes.NewBuffer(body))
		if err != nil {
			errChan <- fmt.Errorf("failed to create http request: %v", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := c.httpClient().Do(httpReq)
		if err != nil {
			select {
			case <-ctx.Done():
			default:
				errChan <- fmt.Errorf("http request failed: %v", err)
			}
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			errChan <- fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(b))
			return
		}

		// Accumulate native tool-call fragments by index; OpenAI streams the
		// name once and the arguments in pieces.
		type acc struct {
			name string
			args strings.Builder
		}
		tools := map[int]*acc{}
		var order []int
		var usage oaiUsage

		emitContent := func(s string) bool {
			if s == "" {
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case respChan <- ChatResponse{Message: Message{Role: "assistant", Content: s}}:
				return true
			}
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			var chunk oaiStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue // skip malformed keep-alive/comment lines
			}
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			ch := chunk.Choices[0]
			if !emitContent(ch.Delta.Content) {
				return
			}
			for _, tc := range ch.Delta.ToolCalls {
				a, ok := tools[tc.Index]
				if !ok {
					a = &acc{}
					tools[tc.Index] = a
					order = append(order, tc.Index)
				}
				if tc.Function.Name != "" {
					a.name = tc.Function.Name
				}
				a.args.WriteString(tc.Function.Arguments)
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				errChan <- fmt.Errorf("error reading MLX stream: %v", err)
				return
			}
		}

		// Terminal chunk: tool calls take priority (the consumer stops the
		// stream and executes them), otherwise a plain Done with usage.
		final := ChatResponse{
			Message:    Message{Role: "assistant"},
			Done:       true,
			PromptEval: usage.PromptTokens,
			EvalCount:  usage.CompletionTokens,
		}
		if len(order) > 0 {
			var calls []oaiToolCall
			for _, idx := range order {
				a := tools[idx]
				var oc oaiToolCall
				oc.Function.Name = a.name
				oc.Function.Arguments = a.args.String()
				calls = append(calls, oc)
			}
			final.Message.ToolCalls = oaiToMCPCalls(calls)
		}
		select {
		case <-ctx.Done():
		case respChan <- final:
		}
	}()

	return respChan, errChan
}
