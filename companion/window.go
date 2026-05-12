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
package companion

import (
	"bufio"
	"encoding/json"
	"image"
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
	InitLog()
	defer CloseLog()
	Logf("companion Run() starting")

	state := &UIState{}

	audioCh, stopAudio := StartAudioCapture()
	if audioCh == nil {
		Logf("audio: capture NOT available; circle will be static and STT disabled")
	}

	// Merged level channel: mic capture and TTS playback both push here so
	// the visualizer reacts to either the user speaking or Layla speaking.
	rawLevelCh := make(chan float32, 64)
	levelCh := LevelMonitor(rawLevelCh, "merged")
	sttCh := make(chan AudioFrame, 32)
	if audioCh != nil {
		go func() {
			defer close(sttCh)
			for f := range audioCh {
				if state.Muted.Load() {
					// Drop mic-derived frames entirely while muted; visualizer
					// still receives TTS levels via the same channel.
					continue
				}
				select {
				case rawLevelCh <- f.Level:
				default:
				}
				select {
				case sttCh <- f:
				default:
				}
			}
			close(rawLevelCh)
		}()
	} else {
		close(sttCh)
		close(rawLevelCh)
	}

	outMsgs := make(chan Message, 16)

	var suppress atomic.Bool
	tts := NewTTS(DefaultTTSConfig(), &suppress, rawLevelCh)

	go RunSTT(sttCh, outMsgs, &suppress, &state.Listening, DefaultSTTConfig())
	go readStdin(tts, state)
	go writeStdout(outMsgs)

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
		if err := loop(w, levelCh, state); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()

	app.Main()
	return nil
}

// readStdin parses one JSON message per line and dispatches.
func readStdin(tts *TTS, state *UIState) {
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

func writeStdout(in <-chan Message) {
	enc := json.NewEncoder(os.Stdout)
	for msg := range in {
		_ = enc.Encode(&msg)
	}
}

// dragTag identifies pointer-event subscriptions; only its address matters.
var dragTag = new(bool)

func loop(w *app.Window, levels <-chan float32, state *UIState) error {
	var ops op.Ops
	start := time.Now()
	var smoothed float32
	var lastBtnRect image.Rectangle

	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return e.Err

		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)

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

			// Single pointer area covers the whole window. On press, we
			// decide: hit-inside-button -> toggle mute; hit-outside-button
			// -> hand the drag off to the compositor.
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
				pe, isPtr := ev.(pointer.Event)
				if !isPtr || pe.Kind != pointer.Press {
					continue
				}
				px := int(pe.Position.X)
				py := int(pe.Position.Y)
				if !lastBtnRect.Empty() && image.Pt(px, py).In(lastBtnRect) {
					was := state.Muted.Load()
					state.Muted.Store(!was)
					Logf("mute toggled -> %v", !was)
				} else {
					w.Perform(system.ActionMove)
				}
			}

			lastBtnRect = Render(gtx, time.Since(start), state, smoothed)

			area.Pop()
			e.Frame(gtx.Ops)
		}
	}
}
