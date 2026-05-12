package companion

import (
	"encoding/binary"
	"io"
	"math"
	"os/exec"
)

// AudioSampleRate is the capture rate in Hz. Whisper.cpp's models are trained
// on 16 kHz audio, so matching that here lets us hand buffers straight to it.
const AudioSampleRate = 16000

// AudioFrameSize is the number of mono int16 samples per AudioFrame
// (~64 ms at 16 kHz).
const AudioFrameSize = 1024

// AudioFrame is one block of captured PCM plus its RMS level.
type AudioFrame struct {
	Samples []int16
	Level   float32 // 0..1
}

// StartAudioCapture spawns `parec` (PulseAudio / PipeWire-pulse compat) to
// read raw 16-bit mono PCM at AudioSampleRate from the default source and
// emits an AudioFrame on the returned channel roughly every 64 ms.
//
// Returns (nil, noop) if parec isn't available — callers should treat that
// as "no audio reactivity / no STT" and continue.
func StartAudioCapture() (<-chan AudioFrame, func()) {
	noop := func() {}

	cmd := exec.Command("parec",
		"--rate=16000",
		"--channels=1",
		"--format=s16le",
		"--raw",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, noop
	}
	if err := cmd.Start(); err != nil {
		return nil, noop
	}

	out := make(chan AudioFrame, 16)

	go func() {
		defer close(out)
		buf := make([]byte, AudioFrameSize*2)
		for {
			if _, err := io.ReadFull(stdout, buf); err != nil {
				return
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
			// Speech RMS sits roughly 0.05..0.3 — scale into a useful range.
			level := float32(rms * 4.0)
			if level > 1 {
				level = 1
			}
			select {
			case out <- AudioFrame{Samples: samples, Level: level}:
			default:
				// Drop if consumers are behind; STT/renderer would rather skip
				// than block the capture pipe.
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
