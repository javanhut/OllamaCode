package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/javanhut/ollama_code/tools"
)

// OpenAI-compatible wire format. Ollama's /api/chat is the native path; this is
// the translation layer for everything else that speaks POST
// /v1/chat/completions — OpenRouter, LM Studio, vLLM, Together, Groq, and
// Ollama's own /v1 shim.
//
// Three things differ from /api/chat, and each is a silent corruption rather
// than an obvious failure if you get it wrong:
//
//   - Tool-call arguments arrive as a JSON *string*, streamed in fragments that
//     must be concatenated per choices[].delta.tool_calls[].index before parsing.
//   - Tool results must carry a tool_call_id matching the assistant call that
//     produced them; Ollama correlates by name and position instead.
//   - The stream is SSE ("data: {...}"), not newline-delimited JSON.

type oaFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type oaToolCall struct {
	Index    *int       `json:"index,omitempty"`
	ID       string     `json:"id,omitempty"`
	Type     string     `json:"type,omitempty"`
	Function oaFunction `json:"function"`
}

// oaMessage keeps Content non-omitempty: providers reject an assistant message
// that carries tool_calls with the content key missing entirely.
type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaJSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
}

type oaResponseFormat struct {
	Type       string        `json:"type"`
	JSONSchema *oaJSONSchema `json:"json_schema,omitempty"`
}

type oaRequest struct {
	Model    string      `json:"model"`
	Messages []oaMessage `json:"messages"`
	Stream   bool        `json:"stream"`
	// tools.Tool already serializes to the OpenAI function-definition shape —
	// Ollama adopted it verbatim — so the definitions pass straight through.
	Tools          []tools.Tool      `json:"tools,omitempty"`
	StreamOptions  *oaStreamOptions  `json:"stream_options,omitempty"`
	Temperature    *float64          `json:"temperature,omitempty"`
	TopP           *float64          `json:"top_p,omitempty"`
	MaxTokens      *int              `json:"max_tokens,omitempty"`
	ResponseFormat *oaResponseFormat `json:"response_format,omitempty"`
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type oaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Reasoning models expose their stream under two different keys
			// depending on the provider; only one is ever populated.
			Reasoning        string       `json:"reasoning"`
			ReasoningContent string       `json:"reasoning_content"`
			ToolCalls        []oaToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
}

type oaCompletion struct {
	Choices []struct {
		Message oaMessage `json:"message"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
}

// toOpenAIMessages converts native history to the OpenAI shape, synthesizing the
// tool_call_id correlation OpenAI requires and Ollama doesn't carry.
//
// ponytail: ids are positional — the Nth pending call is answered by the Nth
// following tool result. That holds because every producer of history appends
// results in call order (pendingBatch.results is indexed by call). If a producer
// ever interleaves them, put an explicit id on tools.ToolCall instead.
//
// Unbalanced pairs are dropped rather than sent: OpenAI providers reject the
// whole request with a 400 when a tool result has no matching call, or a call
// has no result, and a truncated history window can produce either.
func toOpenAIMessages(msgs []Message) []oaMessage {
	out := make([]oaMessage, 0, len(msgs))
	var outstanding []string // ids awaiting a result, oldest first

	for i, msg := range msgs {
		switch msg.Role {
		case "tool":
			if len(outstanding) == 0 {
				continue // orphaned result: its call fell outside the window
			}
			out = append(out, oaMessage{Role: "tool", Content: msg.Content, ToolCallID: outstanding[0]})
			outstanding = outstanding[1:]

		case "assistant":
			m := oaMessage{Role: "assistant", Content: msg.Content}
			if len(msg.ToolCalls) > 0 && toolResultsAfter(msgs, i) >= len(msg.ToolCalls) {
				for j, c := range msg.ToolCalls {
					id := fmt.Sprintf("call_%d_%d", i, j)
					args := strings.TrimSpace(string(c.Function.Arguments))
					if args == "" {
						args = "{}"
					}
					m.ToolCalls = append(m.ToolCalls, oaToolCall{
						ID:       id,
						Type:     "function",
						Function: oaFunction{Name: c.Function.Name, Arguments: args},
					})
					outstanding = append(outstanding, id)
				}
			}
			out = append(out, m)

		default:
			out = append(out, oaMessage{Role: msg.Role, Content: msg.Content})
		}
	}
	return out
}

// toolResultsAfter counts the results answering the call at index i. System
// messages are skipped: the loop guards splice advisories between an assistant's
// tool calls and their results.
func toolResultsAfter(msgs []Message, i int) int {
	n := 0
	for j := i + 1; j < len(msgs); j++ {
		switch msgs[j].Role {
		case "tool":
			n++
		case "system":
			continue
		default:
			return n
		}
	}
	return n
}

// openAIURL joins the configured base with an endpoint path. A base carrying no
// path at all gets /v1 appended — much the commonest way to mis-enter one.
func (o OllamaHost) openAIURL(path string) string {
	base := strings.TrimRight(o.uri, "/")
	if u, err := url.Parse(base); err == nil && strings.Trim(u.Path, "/") == "" {
		base += "/v1"
	}
	return base + path
}

// openAIRequest maps a native ChatRequest onto the OpenAI body. num_ctx has no
// equivalent and is dropped: the server owns the context window there.
func (o OllamaHost) openAIRequest(req ChatRequest, stream bool) oaRequest {
	out := oaRequest{
		Model:    req.Model,
		Messages: toOpenAIMessages(req.Messages),
		Stream:   stream,
		Tools:    req.Tools,
	}
	if stream {
		out.StreamOptions = &oaStreamOptions{IncludeUsage: true}
	}
	if v, ok := req.Options["temperature"].(float64); ok {
		out.Temperature = &v
	}
	if v, ok := req.Options["top_p"].(float64); ok {
		out.TopP = &v
	}
	if v, ok := req.Options["num_predict"].(int); ok && v > 0 {
		out.MaxTokens = &v
	}
	if len(req.Format) > 0 {
		// strict is deliberately unset: it demands additionalProperties:false and
		// every key required, which the repair schemas don't guarantee.
		out.ResponseFormat = &oaResponseFormat{
			Type:       "json_schema",
			JSONSchema: &oaJSONSchema{Name: "result", Schema: req.Format},
		}
	}
	return out
}

// postOpenAI sends the request and returns the response body on success. The
// provider's error text is preserved verbatim — a bare status code turns every
// schema or correlation mistake into an unfalsifiable guess.
func (o OllamaHost) postOpenAI(ctx context.Context, payload oaRequest) (io.ReadCloser, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal chat request: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", o.openAIURL("/chat/completions"), bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if payload.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	o.applyAuth(req)

	resp, err := ollamaHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("unexpected status code: %d: %s", resp.StatusCode, msg)
	}
	return resp.Body, nil
}

func (o OllamaHost) chatOpenAI(ctx context.Context, req ChatRequest) (<-chan ChatResponse, <-chan error) {
	respChan := make(chan ChatResponse)
	errChan := make(chan error, 1)

	go func() {
		defer close(respChan)
		defer close(errChan)

		body, err := o.postOpenAI(ctx, o.openAIRequest(req, true))
		if err != nil {
			select {
			case <-ctx.Done(): // cancelled turn: the caller is gone, not an error
			default:
				errChan <- err
			}
			return
		}
		defer body.Close()
		streamOpenAI(ctx, body, respChan, errChan)
	}()

	return respChan, errChan
}

// streamOpenAI translates an SSE body into native chunks. Tool-call fragments
// accumulate by index and are emitted as one terminal chunk, matching the shape
// /api/chat delivers, so the caller's stream loop needs no OpenAI awareness.
func streamOpenAI(ctx context.Context, body io.Reader, out chan<- ChatResponse, errs chan<- error) {
	emit := func(c ChatResponse) bool {
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// ReadString rather than a Scanner: a single data: frame carrying a large
	// tool-call argument can exceed Scanner's fixed token ceiling.
	reader := bufio.NewReader(body)
	calls := map[int]*oaFunction{}
	var order []int
	var usage oaUsage

	for {
		line, readErr := reader.ReadString('\n')

		if data, ok := sseData(line); ok {
			if data == "[DONE]" {
				break
			}
			var chunk oaStreamChunk
			// Keep-alives and provider-specific frames aren't fatal — skip them.
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				if chunk.Usage != nil {
					usage = *chunk.Usage
				}
				if len(chunk.Choices) > 0 {
					d := chunk.Choices[0].Delta
					// Exactly one reasoning key is ever populated, so summing
					// them is just "whichever this provider used".
					if r := d.Reasoning + d.ReasoningContent; r != "" {
						if !emit(ChatResponse{Message: Message{Role: "assistant", Thinking: r}}) {
							return
						}
					}
					if d.Content != "" {
						if !emit(ChatResponse{Message: Message{Role: "assistant", Content: d.Content}}) {
							return
						}
					}
					for _, tc := range d.ToolCalls {
						idx := 0
						if tc.Index != nil {
							idx = *tc.Index
						}
						cur, ok := calls[idx]
						if !ok {
							cur = &oaFunction{}
							calls[idx] = cur
							order = append(order, idx)
						}
						if tc.Function.Name != "" {
							cur.Name = tc.Function.Name
						}
						cur.Arguments += tc.Function.Arguments
					}
				}
			}
		}

		if readErr != nil {
			if readErr != io.EOF {
				errs <- fmt.Errorf("error reading stream: %v", readErr)
				return
			}
			break
		}
	}

	final := ChatResponse{
		Done:       true,
		Message:    Message{Role: "assistant"},
		PromptEval: usage.PromptTokens,
		EvalCount:  usage.CompletionTokens,
	}
	slices.Sort(order)
	for _, idx := range order {
		c := calls[idx]
		if c.Name == "" {
			continue
		}
		args := strings.TrimSpace(c.Arguments)
		if args == "" {
			args = "{}"
		}
		final.Message.ToolCalls = append(final.Message.ToolCalls, tools.ToolCall{
			Function: tools.ToolCallFunction{Name: c.Name, Arguments: json.RawMessage(args)},
		})
	}
	emit(final)
}

// sseData extracts the payload of a "data:" frame, reporting false for blank
// lines, comments, and the event:/id: framing lines.
func sseData(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" || strings.HasPrefix(line, ":") {
		return "", false
	}
	data, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(data), true
}

func (o OllamaHost) chatOnceOpenAI(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body, err := o.postOpenAI(ctx, o.openAIRequest(req, false))
	if err != nil {
		return ChatResponse{}, err
	}
	defer body.Close()

	var out oaCompletion
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return ChatResponse{}, fmt.Errorf("failed to decode response: %v", err)
	}
	if len(out.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("response contained no choices")
	}

	msg := out.Choices[0].Message
	res := ChatResponse{
		Model:   req.Model,
		Done:    true,
		Message: Message{Role: "assistant", Content: msg.Content},
	}
	for _, c := range msg.ToolCalls {
		args := strings.TrimSpace(c.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		res.Message.ToolCalls = append(res.Message.ToolCalls, tools.ToolCall{
			Function: tools.ToolCallFunction{Name: c.Function.Name, Arguments: json.RawMessage(args)},
		})
	}
	if out.Usage != nil {
		res.PromptEval = out.Usage.PromptTokens
		res.EvalCount = out.Usage.CompletionTokens
	}
	return res, nil
}

// generateOpenAI backs the plain prompt/response path (history compaction) with
// a single-turn chat completion, since /v1 has no separate generate endpoint.
func (o OllamaHost) generateOpenAI(req GenerateRequest) (*GenerateResponse, error) {
	res, err := o.chatOnceOpenAI(context.Background(), ChatRequest{
		Model:    req.Model,
		Messages: []Message{{Role: "user", Content: req.Prompt}},
	})
	if err != nil {
		return nil, err
	}
	return &GenerateResponse{Model: req.Model, Response: res.Message.Content, Done: true}, nil
}

func (o OllamaHost) modelsOpenAI() (*ModelListResponse, error) {
	req, err := http.NewRequest("GET", o.openAIURL("/models"), nil)
	if err != nil {
		return nil, err
	}
	o.applyAuth(req)
	resp, err := ollamaHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("unexpected status code: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("failed to decode model list: %v", err)
	}
	var list ModelListResponse
	for _, d := range payload.Data {
		list.Models = append(list.Models, struct {
			Name string `json:"name"`
		}{Name: d.ID})
	}
	return &list, nil
}
