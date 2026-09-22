package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/javanhut/ollama_code/api"
)

// ReplayClient supplies captured model responses in order, allowing the shared
// headless harness to reproduce dispatch, repair, and loop behavior without a
// live model. Requests are intentionally ignored: parity is asserted by the
// resulting tool trace and outcome checks.
type ReplayClient struct {
	mu        sync.Mutex
	responses []api.ChatResponse
	index     int
}

func NewReplayClient(path string) (*ReplayClient, error) {
	client := &ReplayClient{}
	err := Replay(path, func(event Event) error {
		if event.Kind != "model_response" || len(event.Payload) == 0 {
			return nil
		}
		var response api.ChatResponse
		if err := json.Unmarshal(event.Payload, &response); err != nil {
			return err
		}
		client.responses = append(client.responses, response)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(client.responses) == 0 {
		return nil, fmt.Errorf("trace contains no replayable model responses")
	}
	return client, nil
}

func (c *ReplayClient) ChatOnce(context.Context, api.ChatRequest) (api.ChatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.index >= len(c.responses) {
		return api.ChatResponse{}, fmt.Errorf("replay exhausted after %d responses", c.index)
	}
	response := c.responses[c.index]
	c.index++
	return response, nil
}
