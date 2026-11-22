//go:build ignore

// gen_template_icons draws the macOS menu bar icons (template-*.png): black
// shapes on transparency that macOS tints for the light or dark menu bar,
// so the state is told by the symbol in the ring, not by colour.
//
//	cd cmd/backupzit-tray/icons && go run gen_template_icons.go
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

// size is the bitmap side: the menu bar shows it at 16 points, so 64 pixels
// stay sharp on every Retina scale.
const size = 64

// sub is the supersampling per axis for smooth edges.
const sub = 4

type point struct{ x, y float64 }

// shape tells whether a point (in 0..1 coordinates) is painted.
type shape func(p point) bool

func ring(r, w float64) shape {
	return func(p point) bool {
		d := math.Hypot(p.x-0.5, p.y-0.5)
		return math.Abs(d-r) <= w/2
	}
}

func disc(c point, r float64) shape {
	return func(p point) bool { return math.Hypot(p.x-c.x, p.y-c.y) <= r }
}

// line is a segment with round ends.
func line(a, b point, w float64) shape {
	return func(p point) bool {
		dx, dy := b.x-a.x, b.y-a.y
		t := ((p.x-a.x)*dx + (p.y-a.y)*dy) / (dx*dx + dy*dy)
		t = math.Max(0, math.Min(1, t))
		return math.Hypot(p.x-(a.x+t*dx), p.y-(a.y+t*dy)) <= w/2
	}
}

func union(s ...shape) shape {
	return func(p point) bool {
		for _, f := range s {
			if f(p) {
				return true
			}
		}
		return false
	}
}

// minus paints a but not b (b is cut out of a).
func minus(a, b shape) shape { return func(p point) bool { return a(p) && !b(p) } }

const (
	stroke = 0.09
	radius = 0.42
)

var icons = map[string]shape{
	// ok: a check mark in the ring.
	"template-ok.png": union(ring(radius, stroke),
		line(point{0.31, 0.52}, point{0.45, 0.66}, stroke), line(point{0.45, 0.66}, point{0.70, 0.36}, stroke)),
	// running: an upward arrow (data leaving for the storage).
	"template-running.png": union(ring(radius, stroke),
		line(point{0.5, 0.30}, point{0.5, 0.70}, stroke),
		line(point{0.5, 0.30}, point{0.34, 0.46}, stroke), line(point{0.5, 0.30}, point{0.66, 0.46}, stroke)),
	// warning: an exclamation mark.
	"template-warning.png": union(ring(radius, stroke),
		line(point{0.5, 0.28}, point{0.5, 0.56}, stroke), disc(point{0.5, 0.71}, stroke*0.65)),
	// error: a cross.
	"template-error.png": union(ring(radius, stroke),
		line(point{0.35, 0.35}, point{0.65, 0.65}, stroke), line(point{0.65, 0.35}, point{0.35, 0.65}, stroke)),
	// offline: the ring struck through, with a gap around the stroke.
	"template-offline.png": union(
		minus(ring(radius, stroke), line(point{0.14, 0.86}, point{0.86, 0.14}, stroke*2.6)),
		line(point{0.16, 0.84}, point{0.84, 0.16}, stroke)),
}

func draw(s shape) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			n := 0
			for sy := 0; sy < sub; sy++ {
				for sx := 0; sx < sub; sx++ {
					p := point{(float64(x) + (float64(sx)+0.5)/sub) / size, (float64(y) + (float64(sy)+0.5)/sub) / size}
					if s(p) {
						n++
					}
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{A: uint8(n * 255 / (sub * sub))})
		}
	}
	return img
}

func main() {
	for name, s := range icons {
		f, err := os.Create(name)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, draw(s)); err != nil {
			panic(err)
		}
		if err := f.Close(); err != nil {
			panic(err)
		}
	}
}
