package companion

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	logOnce   sync.Once
	logger    *log.Logger
	logFile   *os.File
	logActive bool
)

// LogPath is where the companion writes its diagnostic log. Always created
// fresh on Run() so each session starts clean.
func LogPath() string {
	return filepath.Join(os.TempDir(), "ollama-companion.log")
}

// InitLog opens the log file and wires the package-level logger. Safe to
// call multiple times.
func InitLog() {
	logOnce.Do(func() {
		f, err := os.Create(LogPath())
		if err != nil {
			return
		}
		logFile = f
		logger = log.New(f, "", log.LstdFlags|log.Lmicroseconds)
		logActive = true
		logger.Printf("companion started (pid=%d)", os.Getpid())
	})
}

// Logf writes a line to the diagnostic log if it's open. Cheap when disabled.
func Logf(format string, args ...any) {
	if !logActive {
		return
	}
	logger.Output(2, fmt.Sprintf(format, args...))
}

// CloseLog flushes the log file.
func CloseLog() {
	if logFile != nil {
		_ = logFile.Close()
	}
}

// LevelMonitor consumes a level channel transparently, sampling peak/avg over
// a fixed window and logging once per second so we can see if audio is
// actually flowing. Returns a passthrough channel.
func LevelMonitor(in <-chan float32, label string) <-chan float32 {
	out := make(chan float32, cap(in))
	go func() {
		defer close(out)
		var (
			peak    float32
			sum     float32
			count   int
			lastLog = time.Now()
		)
		for v := range in {
			if v > peak {
				peak = v
			}
			sum += v
			count++
			out <- v
			if time.Since(lastLog) >= time.Second {
				avg := float32(0)
				if count > 0 {
					avg = sum / float32(count)
				}
				Logf("[%s] level: peak=%.3f avg=%.3f frames=%d", label, peak, avg, count)
				peak = 0
				sum = 0
				count = 0
				lastLog = time.Now()
			}
		}
	}()
	return out
}
