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
