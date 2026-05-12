# Makefile for OllamaCode Cross-Platform Setup (Go Edition)

# --- Configuration ---
PROJECT_NAME := ollama-code
COMPANION_NAME := ollama-companion
VERSION := 1.0.0
MAIN_ENTRY_POINT := main.go # Assuming your main package entry file is main.go

# --- Targets ---

# Default target: setup modules, build, and run
all: setup run

# Target to set up the Go environment (downloads modules and verifies dependencies)
setup:
	@echo "--- Running Setup Phase: Tidy Go Modules for $(PROJECT_NAME) ---"
	go mod tidy || { echo "Error: Failed to run go mod tidy. Ensure you are in the project root."; exit 1; }
	@echo "Setup complete."

# Target to build the cross-platform executable binary
build: setup
	@echo "--- Building $(PROJECT_NAME) Binary ---"
	# Build for current OS/Architecture
	go build -v -o $(PROJECT_NAME) .
	# To cross-compile for Linux on macOS, for example:
	# GOOS=linux GOARCH=amd64 go build -v -o $(PROJECT_NAME)_linux .
	@echo "Build complete. Binary named $(PROJECT_NAME) created."

# Target to install/deploy the built binary (Placeholder)
install: build
	@echo "--- Installation/Deployment Phase ---"
	# TODO: Implement logic to copy $(PROJECT_NAME) binary to system PATH or deployment directory
	@echo "Installation target defined. Please implement specific deployment steps."

# Target to run the application using the compiled binary
run: build
	@echo "--- Running $(PROJECT_NAME) using compiled binary ---"
	./$(PROJECT_NAME)

# Target to run the application during development (no build step)
dev: setup
	@echo "--- Running $(PROJECT_NAME) in development mode ---"
	go run $(MAIN_ENTRY_POINT)

# --- Companion GUI Targets ---

# Build the optional Gio-based popup companion (separate binary).
build-companion:
	@echo "--- Building $(COMPANION_NAME) ---"
	go build -v -o $(COMPANION_NAME) ./cmd/companion
	@echo "Companion build complete: $(COMPANION_NAME)"

# Build then run the companion.
run-companion: build-companion
	./$(COMPANION_NAME)

# Fast iteration: run the companion without producing a binary.
dev-companion:
	go run ./cmd/companion

# Target to clean generated files and directories
clean:
	@echo "--- Cleaning up built files and directories ---"
	rm -f $(PROJECT_NAME) $(COMPANION_NAME) build/ dist/ *.pyc
	@echo "Cleanup complete."

# --- Phony Markers ---
.PHONY: all setup build run dev clean build-companion run-companion dev-companion
