# Makefile for OllamaCode Cross-Platform Setup (Go Edition)

# --- Configuration ---
PROJECT_NAME := ollama-code
COMPANION_NAME := ollama-companion
VERSION := 1.0.0
MAIN_ENTRY_POINT := main.go # Assuming your main package entry file is main.go
UNAME_S := $(shell uname -s)

# Installation locations (override as needed, e.g. `make install PREFIX=$$HOME/.local`)
DEFAULT_PREFIX := /usr/local
ifeq ($(UNAME_S),Darwin)
ifneq ("$(wildcard /opt/homebrew/bin)","")
DEFAULT_PREFIX := /opt/homebrew
endif
endif
PREFIX ?= $(DEFAULT_PREFIX)
BINDIR ?= $(PREFIX)/bin
DESTDIR ?=

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

# Target to install the built binary into PATH.
install: build
	@echo "--- Installing $(PROJECT_NAME) ---"
	@echo "OS: $(UNAME_S)"
	@echo "Install path: $(DESTDIR)$(BINDIR)/$(PROJECT_NAME)"
	mkdir -p "$(DESTDIR)$(BINDIR)"
	install -m 0755 "$(PROJECT_NAME)" "$(DESTDIR)$(BINDIR)/$(PROJECT_NAME)"
	@echo "Install complete."

# Target to uninstall the installed binary.
uninstall:
	@echo "--- Uninstalling $(PROJECT_NAME) ---"
	@if [ -f "$(DESTDIR)$(BINDIR)/$(PROJECT_NAME)" ]; then \
		rm -f "$(DESTDIR)$(BINDIR)/$(PROJECT_NAME)"; \
		echo "Removed $(DESTDIR)$(BINDIR)/$(PROJECT_NAME)"; \
	else \
		echo "No installed binary found at $(DESTDIR)$(BINDIR)/$(PROJECT_NAME)"; \
	fi
	@echo "Uninstall complete."

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

# Install the optional companion binary.
install-companion: build-companion
	@echo "--- Installing $(COMPANION_NAME) ---"
	@echo "OS: $(UNAME_S)"
	@echo "Install path: $(DESTDIR)$(BINDIR)/$(COMPANION_NAME)"
	mkdir -p "$(DESTDIR)$(BINDIR)"
	install -m 0755 "$(COMPANION_NAME)" "$(DESTDIR)$(BINDIR)/$(COMPANION_NAME)"
	@echo "Install complete."

# Uninstall the optional companion binary.
uninstall-companion:
	@echo "--- Uninstalling $(COMPANION_NAME) ---"
	@if [ -f "$(DESTDIR)$(BINDIR)/$(COMPANION_NAME)" ]; then \
		rm -f "$(DESTDIR)$(BINDIR)/$(COMPANION_NAME)"; \
		echo "Removed $(DESTDIR)$(BINDIR)/$(COMPANION_NAME)"; \
	else \
		echo "No installed binary found at $(DESTDIR)$(BINDIR)/$(COMPANION_NAME)"; \
	fi
	@echo "Uninstall complete."

# Target to clean generated files and directories
clean:
	@echo "--- Cleaning up built files and directories ---"
	rm -f $(PROJECT_NAME) $(COMPANION_NAME) build/ dist/ *.pyc
	@echo "Cleanup complete."

# --- Phony Markers ---
.PHONY: all setup build install uninstall run dev clean build-companion run-companion dev-companion install-companion uninstall-companion
