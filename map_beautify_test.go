package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// makeOccupancyPNG builds a PNG that looks like a ROS2 occupancy grid:
// a free (white) interior with a black border (wall) and a gray unknown patch.
func makeOccupancyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var v uint8 = 255 // free
			if x == 0 || y == 0 || x == w-1 || y == h-1 {
				v = 0 // wall
			} else if x > w/2 && y > h/2 {
				v = 128 // unknown
			}
			img.SetGray(x, y, color.Gray{Y: v})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestBeautifyMap(t *testing.T) {
	t.Parallel()
	raw := makeOccupancyPNG(t, 60, 40)
	out, err := BeautifyMap(raw, 0.05)
	if err != nil {
		t.Fatalf("BeautifyMap: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("BeautifyMap returned empty output")
	}
	// Output must be a decodable PNG.
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("BeautifyMap output is not a valid image: %v", err)
	}
	if img.Bounds().Dx() == 0 || img.Bounds().Dy() == 0 {
		t.Fatalf("BeautifyMap output has zero dimensions")
	}

	// resolution <= 0 → defaults to 0.05, should still work.
	if _, err := BeautifyMap(raw, 0); err != nil {
		t.Errorf("BeautifyMap with default resolution: %v", err)
	}

	// Garbage input → error.
	if _, err := BeautifyMap([]byte("not an image"), 0.05); err == nil {
		t.Errorf("BeautifyMap should error on non-image input")
	}
}

func TestDecodePGM(t *testing.T) {
	t.Parallel()
	// Minimal binary PGM (P5): 2x2, maxval 255, pixels 0,128,200,255.
	pgm := append([]byte("P5\n2 2\n255\n"), 0, 128, 200, 255)
	img, err := decodePGM(pgm)
	if err != nil {
		t.Fatalf("decodePGM: %v", err)
	}
	if img.Bounds().Dx() != 2 || img.Bounds().Dy() != 2 {
		t.Fatalf("decodePGM dims = %v, want 2x2", img.Bounds())
	}
	// Ensure BeautifyMap accepts a PGM via its fallback path.
	if _, err := BeautifyMap(pgm, 0.05); err != nil {
		t.Errorf("BeautifyMap on PGM: %v", err)
	}

	if _, err := decodePGM([]byte("P3\n1 1\n255\n0 0 0\n")); err == nil {
		t.Errorf("decodePGM should reject non-P5 formats")
	}
	if _, err := decodePGM([]byte("garbage")); err == nil {
		t.Errorf("decodePGM should reject garbage")
	}
}
