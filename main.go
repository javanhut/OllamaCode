package main

import (
	"fmt"
	"os"

	"github.com/javanhut/ollama_code/tui"
)

func main() {
	if err := tui.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
