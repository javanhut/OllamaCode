package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	var gotRequest ChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Errorf("decode request: %v", err)
		}
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

	var content strings.Builder
	for {
		select {
		case chunk, ok := <-respChan:
			if !ok {
				goto done
			}
			content.WriteString(chunk.Message.Content)
		case err := <-errChan:
			if err != nil {
				t.Fatalf("ContinuousChat failed: %v", err)
			}
		}
	}
done:
	if content.String() != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", content.String())
	}
	if gotRequest.KeepAlive != defaultKeepAlive {
		t.Errorf("keep_alive = %q, want %q", gotRequest.KeepAlive, defaultKeepAlive)
	}
}

func TestContinuousChatIncludesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model needs more memory"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	host := OllamaHost{uri: server.URL}
	responses, errs := host.ContinuousChat(context.Background(), ChatRequest{Model: "too-big"})
	for range responses {
	}
	err := <-errs
	if err == nil || !strings.Contains(err.Error(), "model needs more memory") {
		t.Fatalf("expected actionable server detail, got %v", err)
	}
}

func TestPullModel(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			t.Errorf("expected path /api/pull, got %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		flusher := w.(http.Flusher)
		for _, p := range []PullProgress{
			{Status: "pulling manifest"},
			{Status: "downloading", Digest: "sha256:abc", Total: 1000, Completed: 500},
			{Status: "downloading", Digest: "sha256:abc", Total: 1000, Completed: 1000},
			{Status: "success"},
		} {
			json.NewEncoder(w).Encode(p)
			w.Write([]byte("\n"))
			flusher.Flush()
		}
	}))
	defer server.Close()

	host := OllamaHost{uri: server.URL}
	host.SetAPIKey("secret-token")
	progCh, errCh := host.PullModel(context.Background(), "qwen3-coder")

	var statuses []string
	var maxCompleted, maxTotal int64 // track byte counts like the TUI does (Total > 0)
loop:
	for {
		select {
		case p, ok := <-progCh:
			if !ok {
				break loop
			}
			statuses = append(statuses, p.Status)
			if p.Total > 0 {
				maxCompleted, maxTotal = p.Completed, p.Total
			}
		case err := <-errCh:
			if err != nil {
				t.Fatalf("PullModel failed: %v", err)
			}
		}
	}

	if len(statuses) != 4 || statuses[0] != "pulling manifest" || statuses[3] != "success" {
		t.Fatalf("unexpected status stream: %v", statuses)
	}
	if maxCompleted != 1000 || maxTotal != 1000 {
		t.Errorf("expected to observe 1000/1000 during download, got %d/%d", maxCompleted, maxTotal)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("expected bearer auth header forwarded, got %q", gotAuth)
	}
}

func TestPullModelServerError(t *testing.T) {
	// Ollama reports a bad model name as an {"error":...} object in the stream.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(PullProgress{Error: "model \"nope\" not found"})
	}))
	defer server.Close()

	host := OllamaHost{uri: server.URL}
	progCh, errCh := host.PullModel(context.Background(), "nope")
	for range progCh {
		// drain
	}
	if err := <-errCh; err == nil {
		t.Fatal("expected an error for a not-found model")
	}
}
