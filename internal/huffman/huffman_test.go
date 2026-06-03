package huffman

import (
	"strings"
	"testing"
)

func BenchmarkEncode(b *testing.B) {
	// Generate a realistic 50KB text (simulating chat history)
	baseText := "User: Please analyze this code and optimize it.\nAssistant: Certainly! Let's look at the loop structure and see how we can make it faster. We will start by profiling it.\n"
	text := strings.Repeat(baseText, 350) // ~52KB

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Encode(text)
	}
}

func BenchmarkDecode(b *testing.B) {
	baseText := "User: Please analyze this code and optimize it.\nAssistant: Certainly! Let's look at the loop structure and see how we can make it faster. We will start by profiling it.\n"
	text := strings.Repeat(baseText, 350) // ~52KB
	encoded, root := Encode(text)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Decode(encoded, root)
	}
}

func TestCompressDecompress(t *testing.T) {
	texts := []string{
		"",
		"A",
		"Hello World!",
		strings.Repeat("OllamaCode is awesome! ", 100),
	}

	for _, text := range texts {
		compressed, err := Compress(text)
		if err != nil {
			t.Errorf("Compress failed: %v", err)
			continue
		}
		if text != "" && strings.Contains(compressed.Encoded, "0") && strings.Contains(compressed.Encoded, "1") && !strings.ContainsAny(compressed.Encoded, "23456789abcdefghijklmnopqrstuvwxyz") {
			t.Errorf("Expected base64 compressed data to contain non-binary characters, got: %s", compressed.Encoded)
		}
		decompressed := Decompress(compressed)
		if decompressed != text {
			t.Errorf("Expected decompressed %q, got %q", text, decompressed)
		}
	}
}

func TestLegacyDecompress(t *testing.T) {
	text := "Testing legacy compatibility!"
	encoded, root := Encode(text)

	// Construct a legacy CompressedData structure where Encoded is the raw bit string
	legacy := &CompressedData{
		Encoded: encoded,
		Tree:    root,
	}

	decompressed := Decompress(legacy)
	if decompressed != text {
		t.Errorf("Expected legacy decompressed %q, got %q", text, decompressed)
	}
}
