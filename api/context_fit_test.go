package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const oomBody = `{"error":"llama-server startup failed after projector CPU offload retry: llama-server process has terminated: exit status 1: cudaMalloc failed: out of memory\nllama_init_from_model: failed to initialize the context: failed to allocate buffer for kv cache"}`

// serverThatFitsAt refuses every request asking for more than fits, recording
// the num_ctx of each attempt.
func serverThatFitsAt(fits int, attempts *[]int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		n, _ := numCtxOf(req)
		*attempts = append(*attempts, n)
		if n > fits {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(oomBody))
			return
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: Message{Role: "assistant", Content: "ok"}, Done: true})
	}))
}

func TestChatOnceShrinksContextUntilItFits(t *testing.T) {
	var attempts []int
	server := serverThatFitsAt(8192, &attempts)
	defer server.Close()
	host := OllamaHost{uri: server.URL}

	opts := map[string]any{"num_ctx": 131072, "temperature": 0.0}
	resp, err := host.ChatOnce(context.Background(), ChatRequest{Model: "fits-at-8k", Options: opts})
	if err != nil {
		t.Fatalf("ChatOnce: %v", err)
	}
	if resp.Message.Content != "ok" {
		t.Fatalf("content: %q", resp.Message.Content)
	}
	want := []int{131072, 65536, 32768, 16384, 8192}
	if len(attempts) != len(want) {
		t.Fatalf("attempts: %v want %v", attempts, want)
	}
	for i, n := range want {
		if attempts[i] != n {
			t.Fatalf("attempts: %v want %v", attempts, want)
		}
	}
	if got := host.ContextCeiling("fits-at-8k"); got != 8192 {
		t.Fatalf("ceiling: %d want 8192", got)
	}
	if opts["num_ctx"] != 131072 {
		t.Fatalf("caller's options were mutated: %v", opts)
	}

	// The learned ceiling is applied up front, so the next call loads first try.
	attempts = nil
	if _, err := host.ChatOnce(context.Background(), ChatRequest{Model: "fits-at-8k", Options: opts}); err != nil {
		t.Fatalf("second ChatOnce: %v", err)
	}
	if len(attempts) != 1 || attempts[0] != 8192 {
		t.Fatalf("second attempt: %v want [8192]", attempts)
	}
}

func TestChatOnceStopsAtFloorWhenNothingFits(t *testing.T) {
	var attempts []int
	server := serverThatFitsAt(0, &attempts)
	defer server.Close()
	host := OllamaHost{uri: server.URL}

	_, err := host.ChatOnce(context.Background(), ChatRequest{Model: "fits-never", Options: map[string]any{"num_ctx": 16384}})
	if err == nil {
		t.Fatal("expected the allocation failure to surface")
	}
	if !IsMemoryFailure(err) {
		t.Fatalf("IsMemoryFailure(%v) = false", err)
	}
	if last := attempts[len(attempts)-1]; last != minContextFloor {
		t.Fatalf("stopped at %d want the %d floor (attempts %v)", last, minContextFloor, attempts)
	}
}

func TestIsMemoryFailureIgnoresOtherErrors(t *testing.T) {
	for _, s := range []string{
		`unexpected status code: 500: internal error`,
		`unexpected status code: 400: {"error":"input length exceeds maximum context length"}`,
	} {
		if IsMemoryFailure(errString(s)) {
			t.Fatalf("IsMemoryFailure(%q) = true", s)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
