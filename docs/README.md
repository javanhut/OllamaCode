# OllamaCode documentation

A terminal coding agent that runs on local models, with the option to route the
expensive thinking somewhere else.

## Start here

| | |
|---|---|
| [Getting started](getting-started.md) | Install, first run, picking a model |
| [Modes](modes.md) | explore → plan → write, and why the mode is the safety boundary |
| [Model routing](routing.md) | Big model plans, local model executes |
| [Commands](commands.md) | Every slash command and key binding |
| [Tools](tools.md) | What the model can do, and what each mode allows |
| [Configuration](configuration.md) | `config.json`, environment variables, per-model profiles |
| [Safety](safety.md) | Approval prompts, undo, loop guards, verification |
| [Architecture](architecture.md) | How a turn actually runs, for people changing the code |
| [Voice companion](companion.md) | Optional speech-in, speech-out popup |

## The 60-second version

```sh
make build          # produces ./ocode
./ocode
```

On first run it finds your Ollama daemon and loads the first available model.
Type a task and press Enter.

You start in **explore** mode (read-only). Press `shift+tab` to cycle to
**plan**, then **write**. The model can also move itself between modes, and
every step that changes a file asks you first.

```
┌─ explore ──────┐   ┌─ plan ─────────┐   ┌─ write ────────┐
│ read, search,  │ → │ read + session │ → │ full toolset,  │
│ read-only shell│   │ notes only     │   │ each write     │
│                │   │                │   │ needs approval │
└────────────────┘   └────────────────┘   └────────────────┘
```

## The idea in one paragraph

Small local models are good at mechanical work and bad at deciding what the
work is. OllamaCode splits those. The mode state machine is already the
division — planning happens in plan mode, execution in write mode — so binding
a *different model to each mode* gives you a big model that thinks and a small
local one that types, with a plan handed between them. See
[Model routing](routing.md).

None of that is required. With no routing configured, OllamaCode is an ordinary
single-model agent and behaves exactly as it did before the feature existed.
