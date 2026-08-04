# Getting started

## Requirements

- Go 1.24+ to build
- A running [Ollama](https://ollama.com) daemon, or an API key for Ollama Cloud
- At least one pulled model with tool support

## Install

```sh
make build          # ./ocode
make install        # also copies it onto your PATH
```

`make check` runs the linter and the full test suite.

## First run

```sh
./ocode
```

It reads `~/.config/ollama_code/config.json`, connects to
`http://localhost:11434` (or `$OLLAMA_HOST`), and auto-selects the first model
it finds if none is configured.

If nothing is installed:

```
/models             # opens the picker — press p to pull
```

Pull something with tool support. A good starting pair:

```sh
ollama pull qwen3-coder:30b       # execution
ollama pull nomic-embed-text      # embeddings, for auto-RAG
```

## Picking a model

```
/models             # interactive: browse, switch, pull
/model              # show the active model's settings
/model use qwen3-coder:30b
/model ctx 32768    # override the context window
/model temp 0.2     # override sampling temperature
```

OllamaCode discovers each model's real context length and capabilities from
`/api/show` and caches them per model, so `num_ctx` adapts instead of being
hardcoded. Models under 15B parameters automatically get a compact system
prompt, a trimmed toolset, and low-temperature decoding — small models
hallucinate paths and malform tool JSON at default temperature.

## Your first task

Type what you want. You start in explore mode, which is read-only, so nothing
can be damaged while you get a feel for it:

```
where is the mode switching logic?
```

When you want changes made, `shift+tab` to write mode — or just ask, and the
model will request the switch itself. Every file write shows you a diff and
waits for `y`.

## Cloud models

Ollama Cloud works through the same client. Set a key and use a `-cloud` tag:

```sh
export OLLAMA_API_KEY=...
```

```
/models             # press p, pull gpt-oss:120b-cloud
```

The key can also be entered in `/settings`, where it is masked. Prefer the
environment variable — it keeps the secret out of `config.json`.

## Next

- [Modes](modes.md) — what each mode allows
- [Model routing](routing.md) — using a bigger model for planning only
