# Voice companion

An optional Gio-based GUI binary (`ollama-companion`) that gives the assistant a
face and a voice. It captures your microphone, transcribes utterances with
whisper.cpp, drops the text into the TUI's input box and auto-sends — and speaks
each reply back through piper. A neon-blue orb visualizes mic level, its
perimeter rippling with audio.

Toggle it from inside the TUI with `/companion`.

## System requirements

The companion shells out to local tools and reads its own audio. None are
bundled.

| Component | Purpose | Install (Arch example) |
|---|---|---|
| **PipeWire or PulseAudio** | mic capture (`pw-cat` preferred, `parec` fallback) and playback (`paplay`) | usually preinstalled on modern Linux desktops |
| **whisper.cpp** | speech-to-text | `git clone https://github.com/ggerganov/whisper.cpp && cd whisper.cpp && cmake -B build && cmake --build build -j` |
| A whisper `ggml-*.bin` model | STT weights | `bash models/download-ggml-model.sh base.en` inside the whisper.cpp checkout |
| **piper** | text-to-speech | `yay -S piper-tts-bin`, `pipx install piper-tts`, or a [piper release](https://github.com/rhasspy/piper/releases) |
| A piper voice (`.onnx` + `.onnx.json`) | TTS weights | a pair from [rhasspy/piper-voices](https://huggingface.co/rhasspy/piper-voices) |
| **Gio system deps** | build time only | `vulkan-headers`, plus the usual X11/Wayland dev headers |

## Auto-discovery layout

Put files here and no environment variables are needed:

```
~/.cache/whisper/
├── whisper-cli           # executable, or a symlink to your whisper.cpp build
└── ggml-base.en.bin      # any ggml-*.bin model; first match wins

~/.cache/piper/
├── en_US-lessac-medium.onnx       # any *.onnx voice; first match wins
└── en_US-lessac-medium.onnx.json  # voice config, sets the sample rate
```

The piper binary is also found at `/opt/piper-tts/piper`,
`~/.cache/piper/piper`, `~/.local/bin/piper`, or anywhere on `$PATH` (as
`piper-tts` or `piper`).

To override any of it, see the `OLLAMA_COMPANION_*` variables in
[Configuration](configuration.md#environment-variables).

## Build and run

```sh
make build-companion       # ./ollama-companion
make build                 # ./ocode
./ocode
```

Then inside the TUI:

```
/companion                 # toggle; speak, and your words land in the input
```

Gio pulls a sizable transitive dependency tree. The main `ocode` binary
deliberately does **not** import `companion/`, so `make build` stays Gio-free —
only `make build-companion` pulls it in.

## Window

- **Drag** anywhere to move it (compositor-mediated; X11 and Wayland).
- **Mute** — the small dot in the top-right. Muted, the orb dims, the wave goes
  flat and STT stops. TTS still drives the visualization.
- **Listening indicator** — the orb turns bright cyan with a faint halo while
  voice activity detection is buffering an utterance, then snaps back after
  ~0.7s of silence as the transcript fires.
- **Diagnostics** at `/tmp/ollama-companion.log`. `tail -f` it while using
  `/companion` to see capture stats, VAD events and mute toggles.

## Wayland caveats

Wayland clients cannot self-position or pin themselves on top. Use a compositor
rule — for Hyprland:

```
windowrulev2 = float, class:^(ollama-companion)$
windowrulev2 = pin,   class:^(ollama-companion)$
windowrulev2 = move 100%-240 100%-240, class:^(ollama-companion)$
```

Frameless rendering is honored by KDE, Sway and Hyprland; GNOME/Mutter may still
draw server-side decorations.
