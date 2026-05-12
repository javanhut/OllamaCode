package companion

import (
	"image"
	"math"
	"time"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

// silenceThreshold is the smoothed level below which we draw a plain circle.
const silenceThreshold = 0.04

// DrawCircle paints a single solid blue disc centered in gtx.Constraints.Max.
// When `level` exceeds the silence threshold, the circle's perimeter is
// modulated by a travelling sine wave whose amplitude scales with `level`.
func DrawCircle(gtx layout.Context, elapsed time.Duration, level float32) layout.Dimensions {
	sz := gtx.Constraints.Max
	if sz.X == 0 || sz.Y == 0 {
		return layout.Dimensions{Size: sz}
	}

	cx := float32(sz.X) / 2
	cy := float32(sz.Y) / 2

	short := sz.X
	if sz.Y < short {
		short = sz.Y
	}
	baseR := float32(short) / 3.5

	if level < silenceThreshold {
		rect := image.Rect(
			int(cx-baseR), int(cy-baseR),
			int(cx+baseR), int(cy+baseR),
		)
		paint.FillShape(gtx.Ops, Circle, clip.Ellipse(rect).Op(gtx.Ops))
		gtx.Execute(op.InvalidateCmd{})
		return layout.Dimensions{Size: sz}
	}

	// Trace a wavy perimeter: r(θ, t) = baseR + amp·sin(k·θ + ω·t)
	const steps = 256
	const lobes = 8
	omega := 4.0 * math.Pi // 2 oscillations / second
	amp := float64(baseR) * 0.22 * float64(level)
	t := elapsed.Seconds()

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
	paint.FillShape(gtx.Ops, Circle, clip.Outline{Path: path.End()}.Op())

	gtx.Execute(op.InvalidateCmd{})
	return layout.Dimensions{Size: sz}
}
