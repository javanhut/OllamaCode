package companion

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// TTSConfig points at the piper binary and ONNX voice model.
type TTSConfig struct {
	Bin   string // e.g. "piper"
	Model string // path to a piper voice .onnx
	// SampleRate is the rate piper outputs raw audio at; most piper voices
	// use 22050, some larger voices use 16000. Read from the voice's
	// adjacent .json config when in doubt.
	SampleRate int
}

func DefaultTTSConfig() TTSConfig {
	bin := os.Getenv("OLLAMA_COMPANION_PIPER_BIN")
	if bin == "" {
		bin = "piper"
	}
	model := os.Getenv("OLLAMA_COMPANION_PIPER_MODEL")
	if model == "" {
		home, _ := os.UserHomeDir()
		model = filepath.Join(home, ".cache", "piper", "en_US-lessac-medium.onnx")
	}
	rate := 22050
	return TTSConfig{Bin: bin, Model: model, SampleRate: rate}
}

// TTS owns the piper/paplay subprocess pair for the currently-playing speak
// request. Calling Speak while one is playing kills it and starts a new one,
// so a fresh assistant reply doesn't have to wait for the previous to finish.
type TTS struct {
	cfg      TTSConfig
	suppress *atomic.Bool // signals STT to ignore mic while TTS is talking

	mu      sync.Mutex
	cancel  func()
	playing bool
}

func NewTTS(cfg TTSConfig, suppress *atomic.Bool) *TTS {
	return &TTS{cfg: cfg, suppress: suppress}
}

// Speak vocalizes text. If something is already playing, it is killed.
// Returns immediately; playback happens in a goroutine.
func (t *TTS) Speak(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
	}
	done := make(chan struct{})
	var localCancel func()
	t.cancel = func() {
		if localCancel != nil {
			localCancel()
		}
		<-done
	}
	t.playing = true
	t.mu.Unlock()

	go func() {
		defer close(done)
		t.suppress.Store(true)
		defer t.suppress.Store(false)

		piper := exec.Command(t.cfg.Bin,
			"--model", t.cfg.Model,
			"--output_raw",
		)
		piper.Stdin = strings.NewReader(text)

		rateArg := "--rate=22050"
		if t.cfg.SampleRate > 0 && t.cfg.SampleRate != 22050 {
			rateArg = "--rate=" + intStr(t.cfg.SampleRate)
		}
		paplay := exec.Command("paplay",
			"--raw",
			rateArg,
			"--format=s16le",
			"--channels=1",
		)

		pr, pw := io.Pipe()
		piper.Stdout = pw
		paplay.Stdin = pr

		// Make stderr observable for diagnostics but don't propagate piper's
		// noisy progress logs to our own stderr.
		piper.Stderr = io.Discard
		paplay.Stderr = io.Discard

		killBoth := func() {
			if piper.Process != nil {
				_ = piper.Process.Kill()
			}
			if paplay.Process != nil {
				_ = paplay.Process.Kill()
			}
			pw.Close()
			pr.Close()
		}
		localCancel = killBoth

		if err := paplay.Start(); err != nil {
			return
		}
		if err := piper.Run(); err != nil {
			killBoth()
			return
		}
		pw.Close()
		_ = paplay.Wait()

		t.mu.Lock()
		t.playing = false
		t.cancel = nil
		t.mu.Unlock()
	}()
}

// Stop kills any in-flight playback.
func (t *TTS) Stop() {
	t.mu.Lock()
	c := t.cancel
	t.mu.Unlock()
	if c != nil {
		c()
	}
}

func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
