package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Compaction summarizes through GenerateResponse: num_ctx must reach the
// host, or it truncates the history being summarized to its default window.
func TestGenerateResponseSendsOptions(t *testing.T) {
	var got GenerateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(GenerateResponse{Response: "summary", Done: true})
	}))
	defer server.Close()

	host := OllamaHost{uri: server.URL}
	resp, err := host.GenerateResponse(GenerateRequest{Model: "m", Prompt: "p", Options: map[string]any{"num_ctx": 16384}})
	if err != nil || resp.Response != "summary" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if n, _ := got.Options["num_ctx"].(float64); n != 16384 {
		t.Fatalf("num_ctx not sent: %+v", got.Options)
	}
}

// An error status used to decode into an empty Response that callers took
// for a real, blank answer.
func TestGenerateResponseRejectsErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	host := OllamaHost{uri: server.URL}
	if resp, err := host.GenerateResponse(GenerateRequest{Model: "m", Prompt: "p"}); err == nil {
		t.Fatalf("404 returned no error, resp=%+v", resp)
	}
}
