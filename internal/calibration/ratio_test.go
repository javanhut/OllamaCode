package calibration

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/javanhut/ollama_code/api"
)

// ratioClient answers only the measurement probes: any request without tools
// is a ratio sample and gets a fixed chars-per-token conversion; the behavior
// probes get a plain (incorrect) answer so Run still completes.
type ratioClient struct {
	charsPerToken float64
	sawSamples    int
	err           error
}

func (c *ratioClient) ChatOnce(_ context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	if len(req.Tools) > 0 {
		return api.ChatResponse{Message: api.Message{Content: "4"}}, nil
	}
	if c.err != nil {
		return api.ChatResponse{}, c.err
	}
	chars := 0
	for _, msg := range req.Messages {
		chars += len(msg.Content)
	}
	c.sawSamples++
	return api.ChatResponse{PromptEval: int(math.Round(float64(chars) / c.charsPerToken))}, nil
}

func TestMeasureCharsPerToken(t *testing.T) {
	client := &ratioClient{charsPerToken: 3.5}
	ratio, ok := measureCharsPerToken(context.Background(), client, "model")
	if !ok {
		t.Fatal("measurement failed")
	}
	if math.Abs(ratio-3.5) > 0.1 {
		t.Fatalf("expected ratio near 3.5, got %f", ratio)
	}
	if client.sawSamples != len(ratioSamples) {
		t.Fatalf("expected %d samples, saw %d", len(ratioSamples), client.sawSamples)
	}
}

func TestMeasureCharsPerTokenSkipsWithoutPromptEval(t *testing.T) {
	// PromptEval stays 0, as with endpoints that don't report usage.
	client := &ratioClient{charsPerToken: 0}
	if _, ok := measureCharsPerToken(context.Background(), client, "model"); ok {
		t.Fatal("expected ok=false when prompt_eval_count is never reported")
	}
}

func TestMeasureCharsPerTokenSkipsOnError(t *testing.T) {
	client := &ratioClient{charsPerToken: 4, err: errors.New("boom")}
	if _, ok := measureCharsPerToken(context.Background(), client, "model"); ok {
		t.Fatal("expected ok=false when the host errors")
	}
}

func TestMeasureCharsPerTokenRejectsGarbage(t *testing.T) {
	// 100 chars/token is not a real tokenizer; treat it as a bad count.
	client := &ratioClient{charsPerToken: 100}
	if _, ok := measureCharsPerToken(context.Background(), client, "model"); ok {
		t.Fatal("expected ok=false for an out-of-range ratio")
	}
}

func TestRunRecordsCharsPerToken(t *testing.T) {
	client := &ratioClient{charsPerToken: 4}
	result, err := Run(context.Background(), client, "model", "provider", "runtime")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(result.CharsPerToken-4) > 0.1 {
		t.Fatalf("expected CharsPerToken near 4, got %f", result.CharsPerToken)
	}
}

func TestSaveLoadRoundTripsCharsPerToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := Result{SuiteVersion: SuiteVersion, Model: "m", Provider: "p", Runtime: "r", CharsPerToken: 3.75}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := Load("m", "p", "r", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.CharsPerToken != want.CharsPerToken {
		t.Fatalf("CharsPerToken did not round trip: %f != %f", got.CharsPerToken, want.CharsPerToken)
	}
}
