package companion

import (
	"encoding/binary"
	"io"
	"math"
	"os/exec"
)

// AudioSampleRate is the capture rate in Hz. Whisper.cpp is trained on 16 kHz
// audio, so matching that here lets us hand buffers straight to it.
const AudioSampleRate = 16000

// AudioFrameSize is the number of mono int16 samples per AudioFrame
// (~64 ms at 16 kHz).
const AudioFrameSize = 1024

// AudioFrame is one block of captured PCM plus its RMS level.
type AudioFrame struct {
	Samples []int16
	Level   float32 // 0..1
}

// StartAudioCapture spawns a microphone reader and emits AudioFrames roughly
// every 64 ms. It tries `pw-cat` first (native PipeWire client, reliable on
// PipeWire-only systems) and falls back to `parec` (PulseAudio compat) — on
// some PipeWire setups parec silently captures zero bytes.
//
// Returns (nil, noop) if no capture tool can be started.
func StartAudioCapture() (<-chan AudioFrame, func()) {
	noop := func() {}

	cmd, err := startCaptureCommand()
	if err != nil {
		Logf("audio: startCaptureCommand: %v", err)
		return nil, noop
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		Logf("audio: StdoutPipe: %v", err)
		return nil, noop
	}
	if err := cmd.Start(); err != nil {
		Logf("audio: Start: %v", err)
		return nil, noop
	}
	Logf("audio: capture started (pid=%d)", cmd.Process.Pid)

	out := make(chan AudioFrame, 16)

	go func() {
		defer close(out)
		buf := make([]byte, AudioFrameSize*2)
		var frames int
		var totalBytes int64
		for {
			n, err := io.ReadFull(stdout, buf)
			if err != nil {
				Logf("audio: read loop exited after %d frames / %d bytes: %v", frames, totalBytes, err)
				return
			}
			totalBytes += int64(n)
			frames++
			if frames == 1 {
				Logf("audio: first frame received (%d bytes)", n)
			}
			samples := make([]int16, AudioFrameSize)
			var sum float64
			for i := 0; i < AudioFrameSize; i++ {
				s := int16(binary.LittleEndian.Uint16(buf[i*2:]))
				samples[i] = s
				v := float64(s) / 32768.0
				sum += v * v
			}
			rms := math.Sqrt(sum / float64(AudioFrameSize))
			level := float32(rms * 4.0)
			if level > 1 {
				level = 1
			}
			select {
			case out <- AudioFrame{Samples: samples, Level: level}:
			default:
			}
		}
	}()

	cancel := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}
	return out, cancel
}

// newStderrLogger returns an io.Writer that forwards every line read from a
// child's stderr into our diagnostic log, prefixed by tool name.
func newStderrLogger(tool string) *stderrLogger {
	return &stderrLogger{tool: tool}
}

type stderrLogger struct {
	tool string
	buf  []byte
}

func (s *stderrLogger) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for {
		idx := -1
		for i, b := range s.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := string(s.buf[:idx])
		s.buf = s.buf[idx+1:]
		Logf("[%s] %s", s.tool, line)
	}
	return len(p), nil
}

// startCaptureCommand picks the best available mic reader for this system.
func startCaptureCommand() (*exec.Cmd, error) {
	if p, err := exec.LookPath("pw-cat"); err == nil {
		Logf("audio: using pw-cat at %s", p)
		c := exec.Command("pw-cat",
			"--record",
			"--rate=16000",
			"--channels=1",
			"--format=s16",
			"--raw",
			"-",
		)
		c.Stderr = newStderrLogger("pw-cat")
		return c, nil
	}
	if p, err := exec.LookPath("parec"); err == nil {
		Logf("audio: using parec at %s", p)
		c := exec.Command("parec",
			"--rate=16000",
			"--channels=1",
			"--format=s16le",
			"--raw",
		)
		c.Stderr = newStderrLogger("parec")
		return c, nil
	}
	Logf("audio: NO CAPTURE TOOL FOUND (need pw-cat or parec on PATH)")
	return nil, errNoCaptureTool
}

var errNoCaptureTool = &captureErr{msg: "no audio capture tool found"}

type captureErr struct{ msg string }

func (e *captureErr) Error() string { return e.msg }
