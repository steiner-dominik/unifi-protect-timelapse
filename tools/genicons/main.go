// Command genicons renders the PWA icons.
//
// The icons are generated rather than committed as opaque binaries so that
// anyone can see exactly how they are produced and regenerate them with
// `go run ./tools/genicons`. Drawing happens at 4x and is box-filtered down,
// which gives clean edges without pulling in an image library.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

const outputDir = "internal/web/assets"

var (
	skyTop     = color.NRGBA{R: 0x16, G: 0x2a, B: 0x3f, A: 0xff}
	skyBottom  = color.NRGBA{R: 0x2f, G: 0x6f, B: 0x4f, A: 0xff}
	sun        = color.NRGBA{R: 0xf2, G: 0xc9, B: 0x6b, A: 0xff}
	ridgeBack  = color.NRGBA{R: 0x1b, G: 0x4a, B: 0x39, A: 0xff}
	ridgeFront = color.NRGBA{R: 0x0f, G: 0x2c, B: 0x24, A: 0xff}
)

func main() {
	targets := []struct {
		name     string
		size     int
		maskable bool
	}{
		{"icon-192.png", 192, false},
		{"icon-512.png", 512, false},
		{"icon-maskable-512.png", 512, true},
	}

	for _, target := range targets {
		img := render(target.size, target.maskable)
		if err := writePNG(filepath.Join(outputDir, target.name), img); err != nil {
			log.Fatalf("writing %s: %v", target.name, err)
		}
		log.Printf("wrote %s (%dx%d)", target.name, target.size, target.size)
	}
}

// render draws the icon at 4x and downsamples it. A maskable icon keeps its
// content inside the safe zone, because launchers may crop it to a circle.
func render(size int, maskable bool) *image.NRGBA {
	const scale = 4
	big := image.NewNRGBA(image.Rect(0, 0, size*scale, size*scale))
	dim := float64(size * scale)

	// Content occupies the middle 80% of a maskable icon.
	inset := 0.0
	if maskable {
		inset = dim * 0.1
	}
	contentSize := dim - 2*inset
	radius := contentSize * 0.22
	if maskable {
		// The safe zone is already inside the crop, so square corners are fine.
		radius = 0
	}

	sunCenterX := inset + contentSize*0.70
	sunCenterY := inset + contentSize*0.30
	sunRadius := contentSize * 0.11

	for y := range big.Bounds().Dy() {
		for x := range big.Bounds().Dx() {
			fx, fy := float64(x), float64(y)

			if !insideRoundedRect(fx, fy, inset, inset, contentSize, contentSize, radius) {
				continue
			}

			// Vertical gradient for the sky.
			t := (fy - inset) / contentSize
			pixel := lerp(skyTop, skyBottom, clamp(t*1.15))

			if math.Hypot(fx-sunCenterX, fy-sunCenterY) <= sunRadius {
				pixel = sun
			}
			// The two ridges define the ground themselves; drawing them back to
			// front keeps the nearer silhouette in front of the further one.
			if fy >= ridgeLine(fx, inset, contentSize, 0.66, 0.28, 0.22) {
				pixel = ridgeBack
			}
			if fy >= ridgeLine(fx, inset, contentSize, 0.84, 0.62, 0.24) {
				pixel = ridgeFront
			}

			big.SetNRGBA(x, y, pixel)
		}
	}

	return downsample(big, size, scale)
}

// ridgeLine returns the y coordinate of a mountain silhouette at x, built from
// a single cosine so the shape stays smooth.
func ridgeLine(x, inset, size, base, peak, period float64) float64 {
	u := (x - inset) / size
	wave := (1 + math.Cos((u-peak)/period*math.Pi)) / 2
	if math.Abs(u-peak) > period {
		wave = 0
	}
	return inset + size*(base-wave*0.26)
}

// insideRoundedRect reports whether a point lies inside a rounded rectangle.
func insideRoundedRect(x, y, left, top, width, height, radius float64) bool {
	right, bottom := left+width, top+height
	if x < left || x > right || y < top || y > bottom {
		return false
	}
	if radius <= 0 {
		return true
	}

	cx := math.Min(math.Max(x, left+radius), right-radius)
	cy := math.Min(math.Max(y, top+radius), bottom-radius)
	return math.Hypot(x-cx, y-cy) <= radius
}

// downsample box-filters the oversampled image, which is what produces the
// antialiased edges.
func downsample(src *image.NRGBA, size, scale int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	samples := float64(scale * scale)

	for y := range size {
		for x := range size {
			var r, g, b, a float64
			for dy := range scale {
				for dx := range scale {
					c := src.NRGBAAt(x*scale+dx, y*scale+dy)
					// Weight colour by alpha so transparent pixels do not
					// darken the edges.
					alpha := float64(c.A) / 255
					r += float64(c.R) * alpha
					g += float64(c.G) * alpha
					b += float64(c.B) * alpha
					a += float64(c.A)
				}
			}
			outA := a / samples
			if outA == 0 {
				continue
			}
			weight := a / 255
			dst.SetNRGBA(x, y, color.NRGBA{
				R: uint8(math.Round(r / weight)),
				G: uint8(math.Round(g / weight)),
				B: uint8(math.Round(b / weight)),
				A: uint8(math.Round(outA)),
			})
		}
	}
	return dst
}

func lerp(a, b color.NRGBA, t float64) color.NRGBA {
	return color.NRGBA{
		R: uint8(float64(a.R) + (float64(b.R)-float64(a.R))*t),
		G: uint8(float64(a.G) + (float64(b.G)-float64(a.G))*t),
		B: uint8(float64(a.B) + (float64(b.B)-float64(a.B))*t),
		A: 0xff,
	}
}

func clamp(v float64) float64 {
	return math.Min(math.Max(v, 0), 1)
}

func writePNG(path string, img image.Image) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(file, img); err != nil {
		return err
	}
	return file.Close()
}
