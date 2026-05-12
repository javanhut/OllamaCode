package companion

import (
	"image"
	"sync/atomic"
)

// UIState holds the shared visual + interaction state for the companion
// window. All fields are safe to mutate from any goroutine.
type UIState struct {
	Muted     atomic.Bool // user has hit the mute button
	Listening atomic.Bool // VAD is currently buffering an utterance
}

// MuteButtonRect returns the on-screen rectangle for the mute toggle for a
// window of the given size. Centralized so the renderer and the hit-tester
// agree on its position.
func MuteButtonRect(sz image.Point) image.Rectangle {
	const margin = 14
	const radius = 14
	cx := sz.X - margin - radius
	cy := margin + radius
	return image.Rect(cx-radius, cy-radius, cx+radius, cy+radius)
}
