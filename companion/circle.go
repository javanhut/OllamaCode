package companion

import (
	"image"
	"image/color"
	"math"
	"time"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

// Color states for the orb.
var (
	orbIdle      = Circle                                                    // default deep sky blue
	orbListening = color.NRGBA{R: 0x40, G: 0xE8, B: 0xFF, A: 0xFF}           // bright cyan
	orbMuted     = color.NRGBA{R: 0x44, G: 0x4A, B: 0x55, A: 0xFF}           // dim gray-blue
	haloColor    = color.NRGBA{R: 0x40, G: 0xE8, B: 0xFF, A: 0x50}           // listening halo
	muteOnDot    = color.NRGBA{R: 0xE6, G: 0x4A, B: 0x4A, A: 0xFF}           // red when muted
	muteOffDot   = color.NRGBA{R: 0x90, G: 0x9A, B: 0xB0, A: 0xC0}           // gray when unmuted
	muteRing     = color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0x40}           // subtle outline
)

// Render draws the entire companion frame: background, listening halo, the
// neon orb (with sine-wave perimeter modulated by `level`), and the mute
// button overlay. Returns the mute button rect so the event loop can hit-test.
func Render(gtx layout.Context, elapsed time.Duration, state *UIState, level float32) image.Rectangle {
	sz := gtx.Constraints.Max
	if sz.X == 0 || sz.Y == 0 {
		return image.Rectangle{}
	}

	muted := state.Muted.Load()
	listening := state.Listening.Load() && !muted

	cx := float32(sz.X) / 2
	cy := float32(sz.Y) / 2
	short := sz.X
	if sz.Y < short {
		short = sz.Y
	}
	baseR := float32(short) / 3.5
	t := elapsed.Seconds()

	// Listening halo: faint outer ring drawn before the orb so the orb
	// overlaps it cleanly.
	if listening {
		// Outer halo ring scales subtly with the wave.
		pulse := 0.5 + 0.5*math.Sin(2*math.Pi*t/1.2)
		haloR := baseR * (1.30 + float32(0.05*pulse))
		rect := image.Rect(int(cx-haloR), int(cy-haloR), int(cx+haloR), int(cy+haloR))
		paint.FillShape(gtx.Ops, haloColor, clip.Ellipse(rect).Op(gtx.Ops))
	}

	// Orb color follows state.
	var orbColor color.NRGBA
	switch {
	case muted:
		orbColor = orbMuted
	case listening:
		orbColor = orbListening
	default:
		orbColor = orbIdle
	}

	drawOrb(gtx, cx, cy, baseR, t, level, muted, orbColor)

	btnRect := MuteButtonRect(sz)
	drawMuteButton(gtx, btnRect, muted)

	gtx.Execute(op.InvalidateCmd{})
	return btnRect
}

func drawOrb(gtx layout.Context, cx, cy, baseR float32, t float64, level float32, muted bool, c color.NRGBA) {
	effective := level
	if muted {
		effective = 0
	}
	if effective < 0 {
		effective = 0
	}
	eased := effective * effective * (3 - 2*effective)
	ampRatio := 0.005 + eased*0.30

	const steps = 256
	const lobes = 8
	omega := 4.0 * math.Pi
	amp := float64(baseR) * float64(ampRatio)

	var path clip.Path
	path.Begin(gtx.Ops)
	for i := 0; i <= steps; i++ {
		theta := 2 * math.Pi * float64(i) / float64(steps)
		r := float64(baseR) + amp*math.Sin(float64(lobes)*theta+omega*t)
		x := cx + float32(r*math.Cos(theta))
		y := cy + float32(r*math.Sin(theta))
		if i == 0 {
			path.MoveTo(f32.Pt(x, y))
		} else {
			path.LineTo(f32.Pt(x, y))
		}
	}
	path.Close()
	paint.FillShape(gtx.Ops, c, clip.Outline{Path: path.End()}.Op())
}

// drawMuteButton renders the small clickable dot in the top-right corner.
// Muted -> filled red; unmuted -> dim gray with a subtle white ring.
func drawMuteButton(gtx layout.Context, rect image.Rectangle, muted bool) {
	if muted {
		paint.FillShape(gtx.Ops, muteOnDot, clip.Ellipse(rect).Op(gtx.Ops))
		return
	}
	// Outer thin ring + inner muted-gray fill.
	paint.FillShape(gtx.Ops, muteRing, clip.Ellipse(rect).Op(gtx.Ops))
	inset := 3
	inner := image.Rect(rect.Min.X+inset, rect.Min.Y+inset, rect.Max.X-inset, rect.Max.Y-inset)
	paint.FillShape(gtx.Ops, muteOffDot, clip.Ellipse(inner).Op(gtx.Ops))
}
