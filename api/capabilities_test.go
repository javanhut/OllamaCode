package api

import "testing"

func TestProviderCapabilities(t *testing.T) {
	var native OllamaHost
	if got := native.ProviderCapabilities(); !got.NativeToolCalls || !got.StructuredOutput || !got.Usage {
		t.Fatalf("unexpected native capabilities: %#v", got)
	}
	var cursor OllamaHost
	cursor.SetProvider(ProviderCursor)
	if got := cursor.ProviderCapabilities(); got.NativeToolCalls || got.StructuredOutput {
		t.Fatalf("cursor must not advertise chat wire features: %#v", got)
	}
}
