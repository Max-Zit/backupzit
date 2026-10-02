// Command mkicon renders the BackupZit logo (indigo rounded square with a
// white database symbol) as PNG and ICO files for installers and the tray.
//
//	go run ./tools/mkicon packaging/windows
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// The logo in a 32x32 coordinate space (same as the console's SVG).
func coverage(x, y float64) (bg, fg bool) {
	// Rounded square, radius 8.
	r := 8.0
	cx, cy := math.Max(r, math.Min(32-r, x)), math.Max(r, math.Min(32-r, y))
	if x < 0 || y < 0 || x > 32 || y > 32 || math.Hypot(x-cx, y-cy) > r {
		return false, false
	}
	const w = 2.0
	ellipse := func(ex, ey, rx, ry float64, lowerOnly bool) bool {
		// Distance to the curve, by sampling points on it.
		best := math.Inf(1)
		for i := 0; i < 180; i++ {
			t := float64(i) / 180 * 2 * math.Pi
			px, py := ex+rx*math.Cos(t), ey+ry*math.Sin(t)
			if lowerOnly && py < ey-0.01 {
				continue
			}
			best = math.Min(best, math.Hypot(x-px, y-py))
		}
		return best < w/2
	}
	vline := func(lx, y1, y2 float64) bool { return math.Abs(x-lx) < w/2 && y >= y1 && y <= y2 }
	fg = ellipse(16, 11.5, 7, 2.5, false) || ellipse(16, 16, 7, 2.5, true) || ellipse(16, 20.5, 7, 2.5, true) ||
		vline(9, 11.5, 20.5) || vline(23, 11.5, 20.5)
	return true, fg
}

func render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4 // supersampling
	bgc := color.NRGBA{0x4f, 0x46, 0xe5, 0xff}
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var nb, nf int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * 32 / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) * 32 / float64(size)
					b, f := coverage(x, y)
					if b {
						nb++
					}
					if f {
						nf++
					}
				}
			}
			a := float64(nb) / (ss * ss)
			if a == 0 {
				continue
			}
			t := float64(nf) / float64(nb) // white share inside the square
			mix := func(c uint8) uint8 { return uint8(float64(c)*(1-t) + 255*t + 0.5) }
			img.SetNRGBA(px, py, color.NRGBA{mix(bgc.R), mix(bgc.G), mix(bgc.B), uint8(a*255 + 0.5)})
		}
	}
	return img
}

func main() {
	dir := "."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	var imgs []*image.NRGBA
	for _, s := range []int{16, 20, 24, 32, 40, 48, 64, 256} {
		imgs = append(imgs, render(s))
	}
	must(os.WriteFile(filepath.Join(dir, "backupzit.ico"), encodeICO(imgs), 0o644))
	if len(os.Args) > 2 {
		writeTrayIcons(os.Args[2])
	}
	// Installer bitmaps (WiX UI): dialog 493x312 with an indigo panel on the
	// left (164px) carrying the logo, banner 493x58 with the logo on the right.
	must(writeBMP(filepath.Join(dir, "dialog.bmp"), 493, 312, func(img *image.NRGBA) {
		fill(img, image.Rect(0, 0, 164, 312), color.NRGBA{0x31, 0x2e, 0x81, 0xff}, color.NRGBA{0x4f, 0x46, 0xe5, 0xff})
		fill(img, image.Rect(164, 0, 493, 312), color.NRGBA{0xff, 0xff, 0xff, 0xff}, color.NRGBA{0xff, 0xff, 0xff, 0xff})
		blend(img, render(96), 34, 40)
	}))
	must(writeBMP(filepath.Join(dir, "banner.bmp"), 493, 58, func(img *image.NRGBA) {
		fill(img, img.Rect, color.NRGBA{0xff, 0xff, 0xff, 0xff}, color.NRGBA{0xff, 0xff, 0xff, 0xff})
		blend(img, render(40), 440, 9)
	}))
	var big bytes.Buffer
	png.Encode(&big, render(256))
	must(os.WriteFile(filepath.Join(dir, "backupzit-256.png"), big.Bytes(), 0o644))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// fill paints r with a vertical gradient from top to bottom.
func fill(img *image.NRGBA, r image.Rectangle, top, bottom color.NRGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		t := float64(y-r.Min.Y) / float64(max(r.Dy()-1, 1))
		c := color.NRGBA{lerp(top.R, bottom.R, t), lerp(top.G, bottom.G, t), lerp(top.B, bottom.B, t), 0xff}
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}

func lerp(a, b uint8, t float64) uint8 { return uint8(float64(a)*(1-t) + float64(b)*t + 0.5) }

// blend draws src over img at (x0, y0) using src alpha.
func blend(img, src *image.NRGBA, x0, y0 int) {
	b := src.Bounds()
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			s := src.NRGBAAt(x, y)
			d := img.NRGBAAt(x0+x, y0+y)
			a := float64(s.A) / 255
			img.SetNRGBA(x0+x, y0+y, color.NRGBA{lerp(d.R, s.R, a), lerp(d.G, s.G, a), lerp(d.B, s.B, a), 0xff})
		}
	}
}

// writeBMP writes a 24-bit uncompressed bitmap (what MSI dialogs accept).
func writeBMP(path string, w, h int, draw func(*image.NRGBA)) error {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw(img)
	row := (w*3 + 3) &^ 3
	var b bytes.Buffer
	size := 54 + row*h
	b.WriteString("BM")
	binary.Write(&b, binary.LittleEndian, []uint32{uint32(size), 0, 54, 40})
	binary.Write(&b, binary.LittleEndian, []int32{int32(w), int32(h)})
	binary.Write(&b, binary.LittleEndian, []uint16{1, 24})
	binary.Write(&b, binary.LittleEndian, []uint32{0, uint32(row * h), 2835, 2835, 0, 0})
	for y := h - 1; y >= 0; y-- {
		line := make([]byte, row)
		for x := 0; x < w; x++ {
			c := img.NRGBAAt(x, y)
			line[x*3], line[x*3+1], line[x*3+2] = c.B, c.G, c.R
		}
		b.Write(line)
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// encodeICO builds an ICO with PNG-compressed entries (Windows Vista and
// newer; Windows 7 reads them too).
func encodeICO(imgs []*image.NRGBA) []byte {
	var pngs [][]byte
	for _, im := range imgs {
		var b bytes.Buffer
		png.Encode(&b, im)
		pngs = append(pngs, b.Bytes())
	}
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, [3]uint16{0, 1, uint16(len(imgs))})
	off := 6 + 16*len(imgs)
	for i, im := range imgs {
		s := im.Bounds().Dx()
		dim := uint8(s)
		if s >= 256 {
			dim = 0
		}
		binary.Write(&ico, binary.LittleEndian, struct {
			W, H, Colors, Reserved uint8
			Planes, BPP            uint16
			Size, Offset           uint32
		}{dim, dim, 0, 0, 1, 32, uint32(len(pngs[i])), uint32(off)})
		off += len(pngs[i])
	}
	for _, p := range pngs {
		ico.Write(p)
	}
	return ico.Bytes()
}

// writeTrayIcons writes the notification area icons: the logo with a status
// dot in the lower right corner.
func writeTrayIcons(dir string) {
	states := map[string]color.NRGBA{
		"ok":      {0x16, 0xa3, 0x4a, 0xff},
		"running": {0x25, 0x63, 0xeb, 0xff},
		"warning": {0xf5, 0x9e, 0x0b, 0xff},
		"error":   {0xdc, 0x26, 0x26, 0xff},
		"offline": {0x6b, 0x72, 0x80, 0xff},
	}
	must(os.MkdirAll(dir, 0o755))
	for name, c := range states {
		var imgs []*image.NRGBA
		for _, s := range []int{16, 20, 24, 32, 40, 48} {
			imgs = append(imgs, badge(render(s), c))
		}
		must(os.WriteFile(filepath.Join(dir, "tray-"+name+".ico"), encodeICO(imgs), 0o644))
	}
}

// badge draws a dot with a white ring over the lower right of img.
func badge(img *image.NRGBA, c color.NRGBA) *image.NRGBA {
	s := float64(img.Bounds().Dx())
	r := s * 0.27
	cx, cy := s-r-0.2, s-r-0.2
	ring := math.Max(1, s/16)
	const ss = 4
	for py := 0; py < img.Bounds().Dy(); py++ {
		for px := 0; px < img.Bounds().Dx(); px++ {
			var nIn, nRing int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					d := math.Hypot(float64(px)+(float64(sx)+0.5)/ss-cx, float64(py)+(float64(sy)+0.5)/ss-cy)
					if d <= r {
						nIn++
					} else if d <= r+ring {
						nRing++
					}
				}
			}
			if nIn+nRing == 0 {
				continue
			}
			d := img.NRGBAAt(px, py)
			a := float64(nIn+nRing) / (ss * ss)
			t := float64(nIn) / float64(nIn+nRing) // dot share vs. white ring
			col := color.NRGBA{lerp(255, c.R, t), lerp(255, c.G, t), lerp(255, c.B, t), 0xff}
			outA := a + float64(d.A)/255*(1-a)
			img.SetNRGBA(px, py, color.NRGBA{lerp(d.R, col.R, a), lerp(d.G, col.G, a), lerp(d.B, col.B, a), uint8(outA*255 + 0.5)})
		}
	}
	return img
}
