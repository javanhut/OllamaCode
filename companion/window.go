// Package companion implements the standalone GUI popup for ollama-code.
//
// Wayland caveats (this is a v1; deliberate, accepted limitations):
//   - Always-on-top: Gio exposes no portable API; on Wayland this needs the
//     wlr-layer-shell protocol. Users should pin the window via a compositor
//     rule (e.g. Hyprland: `windowrulev2 = pin, class:^(ollama-companion)$`).
//   - Self-positioning: Wayland clients cannot place themselves; corner
//     placement is a compositor rule.
//   - Frameless: app.Decorated(false) is honored by KDE/Sway/Hyprland; Mutter
//     may still draw server-side decorations.
//   - Drag-to-move: handled via system.ActionMove (compositor-mediated).
//
// IPC: when launched as a child of ollama-code, the companion uses line-delimited
// JSON on its stdin (speak/stop/shutdown) and stdout (ready/transcript/error).
// Run standalone (interactive shell), stdin is a terminal and the bridge is
// effectively idle — STT still emits to stdout, which the user just sees as
// printed JSON.
package companion

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/io/system"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
)

// Run opens the companion window and blocks until it is closed.
// Must be called from main(); it drives the OS event loop via app.Main().
func Run() error {
	audioCh, stopAudio := StartAudioCapture()

	// Fan-out: capture -> (render levels, STT samples)
	levelCh := make(chan float32, 32)
	sttCh := make(chan AudioFrame, 32)
	if audioCh != nil {
		go func() {
			defer close(levelCh)
			defer close(sttCh)
			for f := range audioCh {
				select {
				case levelCh <- f.Level:
				default:
				}
				select {
				case sttCh <- f:
				default:
				}
			}
		}()
	} else {
		close(levelCh)
		close(sttCh)
	}

	// Outbound messages to the parent CLI on stdout.
	outMsgs := make(chan Message, 16)

	// suppress flag: TTS sets it while speaking so STT ignores the speaker echo.
	var suppress atomic.Bool

	tts := NewTTS(DefaultTTSConfig(), &suppress)

	// STT pipeline.
	go RunSTT(sttCh, outMsgs, &suppress, DefaultSTTConfig())

	// Stdin reader: parent CLI -> companion (speak / stop / shutdown).
	go readStdin(tts)

	// Stdout writer: companion -> parent CLI.
	go writeStdout(outMsgs)

	// Announce readiness once everything is wired.
	select {
	case outMsgs <- Message{Type: MsgReady}:
	default:
	}

	go func() {
		defer stopAudio()
		w := new(app.Window)
		w.Option(
			app.Title("ollama-companion"),
			app.Size(unit.Dp(220), unit.Dp(220)),
			app.MinSize(unit.Dp(220), unit.Dp(220)),
			app.MaxSize(unit.Dp(220), unit.Dp(220)),
			app.Decorated(false),
		)
		if err := loop(w, levelCh); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()

	app.Main()
	return nil
}

// readStdin parses one JSON message per line and dispatches.
func readStdin(tts *TTS) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case MsgSpeak:
			tts.Speak(msg.Text)
		case MsgStop:
			tts.Stop()
		case MsgShutdown:
			tts.Stop()
			os.Exit(0)
		}
	}
}

// writeStdout emits one JSON message per line.
func writeStdout(in <-chan Message) {
	enc := json.NewEncoder(os.Stdout)
	for msg := range in {
		_ = enc.Encode(&msg)
	}
}

// dragTag identifies pointer-event subscriptions; only its address matters.
var dragTag = new(bool)

func loop(w *app.Window, levels <-chan float32) error {
	var ops op.Ops
	start := time.Now()
	var smoothed float32

	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return e.Err

		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)

			// Drain available levels; keep the latest values.
			if levels != nil {
			drain:
				for {
					select {
					case lvl, ok := <-levels:
						if !ok {
							levels = nil
							break drain
						}
						if lvl > smoothed {
							smoothed = smoothed*0.4 + lvl*0.6
						} else {
							smoothed = smoothed*0.85 + lvl*0.15
						}
					default:
						break drain
					}
				}
			}

			paint.Fill(gtx.Ops, Background)

			// Whole-window drag handle.
			area := clip.Rect{Max: e.Size}.Push(gtx.Ops)
			event.Op(gtx.Ops, dragTag)
			for {
				ev, ok := gtx.Event(pointer.Filter{
					Target: dragTag,
					Kinds:  pointer.Press,
				})
				if !ok {
					break
				}
				if pe, ok := ev.(pointer.Event); ok && pe.Kind == pointer.Press {
					w.Perform(system.ActionMove)
				}
			}

			DrawCircle(gtx, time.Since(start), smoothed)

			area.Pop()
			e.Frame(gtx.Ops)
		}
	}
}
