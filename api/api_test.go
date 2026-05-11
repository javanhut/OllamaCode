package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetModelList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("expected path /api/tags, got %s", r.URL.Path)
		}
		resp := ModelListResponse{
			Models: []struct {
				Name string `json:"name"`
			}{
				{Name: "llama3"},
				{Name: "mistral"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	host := OllamaHost{uri: server.URL}
	list, err := host.GetModelList()
	if err != nil {
		t.Fatalf("GetModelList failed: %v", err)
	}

	if len(list.Models) != 2 {
		t.Errorf("expected 2 models, got %d", len(list.Models))
	}
	if list.Models[0].Name != "llama3" {
		t.Errorf("expected model llama3, got %s", list.Models[0].Name)
	}
}

func TestContinuousChat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher")
		}

		resp1 := ChatResponse{
			Message: Message{Role: "assistant", Content: "Hello"},
			Done:    false,
		}
		resp2 := ChatResponse{
			Message: Message{Role: "assistant", Content: " world"},
			Done:    true,
		}

		json.NewEncoder(w).Encode(resp1)
		w.(http.ResponseWriter).Write([]byte("\n"))
		flusher.Flush()
		json.NewEncoder(w).Encode(resp2)
		w.(http.ResponseWriter).Write([]byte("\n"))
		flusher.Flush()
	}))
	defer server.Close()

	host := OllamaHost{uri: server.URL}
	ctx := context.Background()
	respChan, errChan := host.ContinuousChat(ctx, ChatRequest{Model: "test"})

	var content string
	for {
		select {
		case chunk, ok := <-respChan:
			if !ok {
				goto done
			}
			content += chunk.Message.Content
		case err := <-errChan:
			if err != nil {
				t.Fatalf("ContinuousChat failed: %v", err)
			}
		}
	}
done:
	if content != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", content)
	}
}
