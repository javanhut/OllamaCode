package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunningModelMatchesBareName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			t.Errorf("path = %s, want /api/ps", r.URL.Path)
		}
		w.Write([]byte(`{"models":[
			{"name":"other:7b","model":"other:7b","size":1,"size_vram":1},
			{"name":"qwen3.8:latest","model":"qwen3.8:latest","size":20000,"size_vram":15000,"context_length":65536}]}`))
	}))
	defer server.Close()
	host := OllamaHost{uri: server.URL}

	rm, ok, err := host.RunningModel("qwen3.8")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rm.Size != 20000 || rm.SizeVRAM != 15000 || rm.ContextLength != 65536 {
		t.Fatalf("rm = %+v", rm)
	}
	if _, ok, _ := host.RunningModel("absent"); ok {
		t.Fatal("an unloaded model was reported as running")
	}
}

func TestRunningModelSkipsOpenAIHosts(t *testing.T) {
	host := OllamaHost{uri: "http://127.0.0.1:1"}
	host.SetProvider("openai")
	if _, ok, err := host.RunningModel("x"); ok || err != nil {
		t.Fatalf("openai host: ok=%v err=%v, want a silent skip", ok, err)
	}
}

func TestChatResponseDecodesCacheFields(t *testing.T) {
	var r ChatResponse
	json.Unmarshal([]byte(`{"done":true,"prompt_eval_count":900,"prompt_eval_cached_count":850,"load_duration":2500000000}`), &r)
	if r.PromptEvalCached != 850 || r.LoadDuration != 2_500_000_000 {
		t.Fatalf("r = %+v", r)
	}
}

func TestSetsSamplingReadsModelfileParameters(t *testing.T) {
	cases := map[string]bool{
		"temperature                    0.6\ntop_k                          20": true,
		"stop                           \"<|im_end|>\"":                         false,
		"":                                    false,
		"num_ctx 4096\nmin_p 0":               true,
		"repeat_penalty 1\nnum_predict 32768": false,
	}
	for params, want := range cases {
		r := ShowModelResponse{Parameters: params}
		if got := r.SetsSampling(); got != want {
			t.Errorf("SetsSampling(%q) = %v, want %v", params, got, want)
		}
	}
}
