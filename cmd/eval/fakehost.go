package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// scriptCall is one scripted tool call: a tool name plus a JSON arguments
// object, exactly as a model would emit it.
type scriptCall struct {
	Name string
	Args string
}

// scriptStep is one model turn in a fixture script: either a batch of tool
// calls, or — when Calls is empty — the final prose answer.
type scriptStep struct {
	Calls   []scriptCall
	Content string
}

// scriptedClient is an in-process agent.ChatClient that plays a fixture's
// Script back turn by turn. It powers -selftest: the fixtures, agent loop,
// tool execution, scoring, and aggregation all run for real, but no Ollama
// host (or network) is needed, so CI can gate on fixture self-consistency.
type scriptedClient struct {
	steps  []scriptStep
	repair string // JSON arguments object returned for a format-repair request
	pos    int
}

// ChatOnce implements agent.ChatClient. Format-repair requests (a JSON schema
// with no tools, see agent.RepairArgsViaFormat) are answered with the fixture's
// RepairArgs; anything else pops the next scripted turn. With the script
// exhausted the client returns an empty prose answer, which ends the loop.
func (c *scriptedClient) ChatOnce(_ context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	if len(req.Format) > 0 && len(req.Tools) == 0 {
		if c.repair == "" {
			return api.ChatResponse{}, fmt.Errorf("scripted client: no repair arguments for format request")
		}
		return api.ChatResponse{Message: api.Message{Role: "assistant", Content: c.repair}}, nil
	}
	if c.pos >= len(c.steps) {
		return api.ChatResponse{Message: api.Message{Role: "assistant"}}, nil
	}
	step := c.steps[c.pos]
	c.pos++
	msg := api.Message{Role: "assistant", Content: step.Content}
	for _, call := range step.Calls {
		args := json.RawMessage(call.Args)
		if !json.Valid(args) {
			return api.ChatResponse{}, fmt.Errorf("scripted client: invalid arguments JSON for %s: %s", call.Name, call.Args)
		}
		msg.ToolCalls = append(msg.ToolCalls, tools.ToolCall{Function: tools.ToolCallFunction{Name: call.Name, Arguments: args}})
	}
	return api.ChatResponse{Message: msg}, nil
}
