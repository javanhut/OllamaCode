package companion

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// STT VAD constants. Tuned for 64 ms frames at 16 kHz.
const (
	sttSpeechThreshold  = 0.06 // RMS above -> speech
	sttSilenceThreshold = 0.04 // RMS below -> silence
	sttSilenceFrames    = 11   // ~0.7 s of silence to close an utterance (snappier turn-taking)
	sttMinSpeechFrames  = 5    // ~320 ms minimum (rejects clicks)
	sttMaxSpeechFrames  = 220  // ~14 s safety cap
	sttPrerollFrames    = 3    // ~192 ms of audio kept before speech onset
)

// STTConfig points at the whisper.cpp binary and model.
type STTConfig struct {
	Bin   string // e.g. "whisper-cli"
	Model string // path to ggml-*.bin
}

// DefaultSTTConfig resolves binary and model paths from, in order:
//
//  1. env vars OLLAMA_COMPANION_WHISPER_{BIN,MODEL}
//  2. ~/.cache/whisper/{whisper-cli,main}     (binary)
//  3. PATH lookup for whisper-cli or main     (binary)
//  4. first ggml-*.bin in ~/.cache/whisper/   (model)
//  5. ~/.cache/whisper/ggml-base.en.bin       (model fallback)
//
// If neither binary nor model can be located, transcription requests will
// fail at runtime and the companion emits MsgError to the parent CLI.
func DefaultSTTConfig() STTConfig {
	home, _ := os.UserHomeDir()
	cache := filepath.Join(home, ".cache", "whisper")

	bin := os.Getenv("OLLAMA_COMPANION_WHISPER_BIN")
	if bin == "" {
		for _, name := range []string{"whisper-cli", "main"} {
			p := filepath.Join(cache, name)
			if isExecutable(p) {
				bin = p
				break
			}
		}
	}
	if bin == "" {
		bin = "whisper-cli" // fall back to PATH lookup at exec time
	}

	model := os.Getenv("OLLAMA_COMPANION_WHISPER_MODEL")
	if model == "" {
		if matches, _ := filepath.Glob(filepath.Join(cache, "ggml-*.bin")); len(matches) > 0 {
			model = matches[0]
		}
	}
	if model == "" {
		model = filepath.Join(cache, "ggml-base.en.bin")
	}

	return STTConfig{Bin: bin, Model: model}
}

func isExecutable(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// RunSTT consumes audio frames, runs voice activity detection, and on each
// detected utterance shells out to whisper.cpp to transcribe. Transcripts
// (and non-fatal errors) are sent on out.
//
// suppress is checked at each frame; while it is non-zero (TTS is playing)
// VAD is reset so the speaker output isn't picked up as user speech.
// listening, if non-nil, mirrors the VAD's "buffering an utterance" state so
// the UI can show a live indicator.
func RunSTT(frames <-chan AudioFrame, out chan<- Message, suppress, listening *atomic.Bool, cfg STTConfig) {
	var (
		preroll      [][]int16 // ring buffer of pre-speech samples
		prerollHead  int
		buf          []int16
		silenceCount int
		speechFrames int
		inSpeech     bool
	)
	preroll = make([][]int16, sttPrerollFrames)
	setListening := func(v bool) {
		if listening != nil {
			listening.Store(v)
		}
	}

	for f := range frames {
		if suppress != nil && suppress.Load() {
			// TTS is talking; keep capture going but never emit transcripts.
			if inSpeech {
				setListening(false)
			}
			inSpeech = false
			buf = buf[:0]
			silenceCount = 0
			speechFrames = 0
			continue
		}

		if !inSpeech {
			// Always keep a small rolling preroll so we don't clip the first phoneme.
			preroll[prerollHead] = f.Samples
			prerollHead = (prerollHead + 1) % sttPrerollFrames

			if f.Level >= sttSpeechThreshold {
				inSpeech = true
				setListening(true)
				buf = buf[:0]
				for i := 0; i < sttPrerollFrames; i++ {
					idx := (prerollHead + i) % sttPrerollFrames
					if preroll[idx] != nil {
						buf = append(buf, preroll[idx]...)
					}
				}
				buf = append(buf, f.Samples...)
				silenceCount = 0
				speechFrames = 1
			}
			continue
		}

		buf = append(buf, f.Samples...)
		speechFrames++
		if f.Level < sttSilenceThreshold {
			silenceCount++
		} else {
			silenceCount = 0
		}

		if silenceCount >= sttSilenceFrames || speechFrames >= sttMaxSpeechFrames {
			if speechFrames >= sttMinSpeechFrames {
				go transcribeAsync(append([]int16(nil), buf...), cfg, out)
			}
			inSpeech = false
			setListening(false)
			buf = buf[:0]
			silenceCount = 0
			speechFrames = 0
		}
	}
	setListening(false)
}

func transcribeAsync(samples []int16, cfg STTConfig, out chan<- Message) {
	text, err := transcribe(samples, cfg)
	if err != nil {
		select {
		case out <- Message{Type: MsgError, Text: "stt: " + err.Error()}:
		default:
		}
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	select {
	case out <- Message{Type: MsgTranscript, Text: text}:
	default:
	}
}

// transcribe writes samples to a temp WAV and shells out to whisper-cli.
func transcribe(samples []int16, cfg STTConfig) (string, error) {
	tmp, err := os.CreateTemp("", "ollama-companion-*.wav")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := writeWAV(tmp, samples, AudioSampleRate); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()

	cmd := exec.Command(cfg.Bin,
		"-m", cfg.Model,
		"-f", tmpName,
		"-nt", // no timestamps
		"-np", // no per-step progress
		"-l", "auto",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	// whisper-cli with -nt -np usually prints just the recognized text, but
	// some builds still emit a "[blank_audio]" line — filter it.
	var lines []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "[") {
			continue
		}
		lines = append(lines, s)
	}
	return strings.Join(lines, " "), nil
}

// writeWAV emits a canonical PCM WAVE (16-bit, mono, sampleRate Hz).
func writeWAV(w *os.File, samples []int16, sampleRate int) error {
	const (
		numChannels   = 1
		bitsPerSample = 16
	)
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	dataSize := len(samples) * 2
	chunkSize := 36 + dataSize

	put := func(b []byte) error { _, err := w.Write(b); return err }
	u32 := func(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	u16 := func(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }

	if err := put([]byte("RIFF")); err != nil {
		return err
	}
	if err := put(u32(uint32(chunkSize))); err != nil {
		return err
	}
	if err := put([]byte("WAVEfmt ")); err != nil {
		return err
	}
	if err := put(u32(16)); err != nil { // PCM fmt chunk size
		return err
	}
	if err := put(u16(1)); err != nil { // PCM
		return err
	}
	if err := put(u16(numChannels)); err != nil {
		return err
	}
	if err := put(u32(uint32(sampleRate))); err != nil {
		return err
	}
	if err := put(u32(uint32(byteRate))); err != nil {
		return err
	}
	if err := put(u16(uint16(blockAlign))); err != nil {
		return err
	}
	if err := put(u16(bitsPerSample)); err != nil {
		return err
	}
	if err := put([]byte("data")); err != nil {
		return err
	}
	if err := put(u32(uint32(dataSize))); err != nil {
		return err
	}
	return binary.Write(w, binary.LittleEndian, samples)
}
