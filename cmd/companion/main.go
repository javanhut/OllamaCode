package main

import (
	"fmt"
	"os"

	"github.com/javanhut/ollama_code/companion"
)

func main() {
	if err := companion.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ollama-companion:", err)
		os.Exit(1)
	}
}
