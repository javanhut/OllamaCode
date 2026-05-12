package companion

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
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

// DefaultTTSConfig resolves binary and model paths from, in order:
//
//  1. env vars OLLAMA_COMPANION_PIPER_{BIN,MODEL}
//  2. ~/.cache/piper/piper                  (binary)
//  3. ~/.local/bin/piper                    (binary)
//  4. /opt/piper-tts/piper                  (binary, AUR piper-tts-bin)
//  5. PATH lookup for piper-tts or piper    (binary)
//  6. first *.onnx in ~/.cache/piper/       (model)
//
// SampleRate defaults to 22050; if the picked voice has an adjacent
// <voice>.onnx.json with a different sample_rate, we honor it.
func DefaultTTSConfig() TTSConfig {
	home, _ := os.UserHomeDir()
	pcache := filepath.Join(home, ".cache", "piper")

	bin := os.Getenv("OLLAMA_COMPANION_PIPER_BIN")
	if bin == "" {
		candidates := []string{
			filepath.Join(pcache, "piper"),
			filepath.Join(home, ".local", "bin", "piper"),
			"/opt/piper-tts/piper",
		}
		for _, p := range candidates {
			if isExecutable(p) {
				bin = p
				break
			}
		}
	}
	if bin == "" {
		if p, err := exec.LookPath("piper-tts"); err == nil {
			bin = p
		} else if p, err := exec.LookPath("piper"); err == nil {
			bin = p
		} else {
			bin = "piper-tts"
		}
	}

	model := os.Getenv("OLLAMA_COMPANION_PIPER_MODEL")
	if model == "" {
		if matches, _ := filepath.Glob(filepath.Join(pcache, "*.onnx")); len(matches) > 0 {
			model = matches[0]
		}
	}

	rate := 22050
	if model != "" {
		if r, ok := readPiperSampleRate(model + ".json"); ok {
			rate = r
		}
	}
	return TTSConfig{Bin: bin, Model: model, SampleRate: rate}
}

func readPiperSampleRate(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var cfg struct {
		Audio struct {
			SampleRate int `json:"sample_rate"`
		} `json:"audio"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return 0, false
	}
	if cfg.Audio.SampleRate <= 0 {
		return 0, false
	}
	return cfg.Audio.SampleRate, true
}

// TTS owns the piper/paplay subprocess pair for the currently-playing speak
// request. Calling Speak while one is playing kills it and starts a new one.
type TTS struct {
	cfg      TTSConfig
	suppress *atomic.Bool  // STT ignores mic while TTS is talking
	levels   chan<- float32 // RMS of TTS audio for the visualizer

	mu      sync.Mutex
	cancel  func()
	playing bool
}

func NewTTS(cfg TTSConfig, suppress *atomic.Bool, levels chan<- float32) *TTS {
	return &TTS{cfg: cfg, suppress: suppress, levels: levels}
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
			"--latency-msec=80", // default is ~1s; this drops first-audio delay
			"--process-time-msec=20",
		)

		piperOut, err := piper.StdoutPipe()
		if err != nil {
			return
		}
		paplayIn, err := paplay.StdinPipe()
		if err != nil {
			return
		}

		piper.Stderr = io.Discard
		paplay.Stderr = io.Discard

		killAll := func() {
			if piper.Process != nil {
				_ = piper.Process.Kill()
			}
			if paplay.Process != nil {
				_ = paplay.Process.Kill()
			}
			_ = paplayIn.Close()
		}
		localCancel = killAll

		if err := paplay.Start(); err != nil {
			return
		}
		if err := piper.Start(); err != nil {
			killAll()
			return
		}

		// Pump piper -> paplay, tapping RMS every chunkSamples samples.
		const chunkSamples = 2048 // ~93 ms at 22050 Hz
		const chunkBytes = chunkSamples * 2
		var leftover []byte
		buf := make([]byte, chunkBytes)
		for {
			n, rerr := piperOut.Read(buf)
			if n > 0 {
				if _, werr := paplayIn.Write(buf[:n]); werr != nil {
					break
				}
				leftover = append(leftover, buf[:n]...)
				for len(leftover) >= chunkBytes {
					emitLevel(leftover[:chunkBytes], t.levels)
					leftover = leftover[chunkBytes:]
				}
			}
			if rerr != nil {
				break
			}
		}
		if len(leftover) >= 2 {
			emitLevel(leftover, t.levels)
		}
		_ = paplayIn.Close()
		_ = piper.Wait()
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

// emitLevel computes RMS over a raw 16-bit LE PCM chunk and forwards it to
// the visualizer channel, non-blocking.
func emitLevel(b []byte, out chan<- float32) {
	if out == nil {
		return
	}
	n := len(b) / 2
	if n == 0 {
		return
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := int16(binary.LittleEndian.Uint16(b[i*2:]))
		v := float64(s) / 32768.0
		sum += v * v
	}
	rms := math.Sqrt(sum / float64(n))
	// Piper is well-leveled (no quiet background), so a tighter gain than
	// the mic path keeps the visualization in a similar range.
	level := float32(rms * 2.5)
	if level > 1 {
		level = 1
	}
	select {
	case out <- level:
	default:
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
