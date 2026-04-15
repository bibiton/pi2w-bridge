package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg" // register JPEG decoder
	"image/png"
	"math"
)

// BeautifyMap converts a ROS2 OccupancyGrid PNG into a clean floor plan
// with grid lines, wall thickening, contour borders and a scale bar.
// Matches genio_helper.py beautify_map output.
//
// resolution: map resolution in m/pixel (default 0.05 if <= 0).
func BeautifyMap(rawImage []byte, resolution float64) ([]byte, error) {
	if resolution <= 0 {
		resolution = 0.05
	}

	// Decode image
	img, _, err := image.Decode(bytes.NewReader(rawImage))
	if err != nil {
		img, err = decodePGM(rawImage)
		if err != nil {
			return nil, fmt.Errorf("cannot decode image: %w", err)
		}
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	// Classify pixels: ROS2 PGM convention
	// low values (<80) = wall/occupied, high values (>220) = free, middle = unknown
	grayVals := make([]uint8, w*h)
	wallMask := make([]bool, w*h)
	freeMask := make([]bool, w*h)

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			gray := uint8((r/256 + g/256 + b/256) / 3)
			idx := (y-bounds.Min.Y)*w + (x - bounds.Min.X)
			grayVals[idx] = gray
			if gray < 80 {
				wallMask[idx] = true
			}
			if gray > 220 {
				freeMask[idx] = true
			}
		}
	}

	// Morphological close on wall mask (3x3, 1 iteration) — fill small gaps
	wallClosed := morphClose(wallMask, w, h)

	// Remove tiny wall clusters (< 8 pixels) — noise removal
	removeSmallClusters(wallClosed, w, h, 8)

	// Thicken walls with 3x3 dilation (wall_px=2 effect)
	wallThick := dilate(wallClosed, w, h)

	// Render canvas
	dst := image.NewRGBA(image.Rect(0, 0, w, h))

	bgColor := color.RGBA{245, 245, 248, 255}
	freeColor := color.RGBA{255, 255, 255, 255}
	wallColor := color.RGBA{40, 40, 50, 255}
	gridColor := color.RGBA{238, 238, 242, 255}
	borderColor := color.RGBA{220, 220, 225, 255}

	// Fill background
	draw.Draw(dst, dst.Bounds(), &image.Uniform{bgColor}, image.Point{}, draw.Src)

	// Draw free space
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if freeMask[y*w+x] {
				dst.SetRGBA(x, y, freeColor)
			}
		}
	}

	// Draw grid lines on free space only
	gridPx := int(1.0 / resolution)
	if gridPx >= 10 {
		for x := 0; x < w; x += gridPx {
			for y := 0; y < h; y++ {
				if freeMask[y*w+x] {
					dst.SetRGBA(x, y, gridColor)
				}
			}
		}
		for y := 0; y < h; y += gridPx {
			for x := 0; x < w; x++ {
				if freeMask[y*w+x] {
					dst.SetRGBA(x, y, gridColor)
				}
			}
		}
	}

	// Draw free space border (1px outline of free region)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if !freeMask[y*w+x] {
				continue
			}
			// Check if on edge of free space
			isEdge := false
			for dy := -1; dy <= 1 && !isEdge; dy++ {
				for dx := -1; dx <= 1 && !isEdge; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					nx, ny := x+dx, y+dy
					if nx < 0 || nx >= w || ny < 0 || ny >= h || !freeMask[ny*w+nx] {
						isEdge = true
					}
				}
			}
			if isEdge {
				dst.SetRGBA(x, y, borderColor)
			}
		}
	}

	// Draw walls (thickened)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if wallThick[y*w+x] {
				dst.SetRGBA(x, y, wallColor)
			}
		}
	}

	// Draw scale bar (1m) at bottom-right
	barLen := int(1.0 / resolution)
	margin := 15
	if barLen > 0 && barLen < w/2 {
		by := h - margin
		bx1 := w - margin - barLen
		bx2 := w - margin
		barColor := color.RGBA{80, 80, 90, 255}
		// Horizontal line
		for x := bx1; x <= bx2; x++ {
			setThickPixel(dst, x, by, barColor)
		}
		// End caps
		for y := by - 4; y <= by+4; y++ {
			setThickPixel(dst, bx1, y, barColor)
			setThickPixel(dst, bx2, y, barColor)
		}
		// "1m" text — simple 3x5 pixel font
		drawText1m(dst, bx1+barLen/2-5, by-12, barColor)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// morphClose performs morphological close (dilate then erode) with 3x3 kernel.
func morphClose(mask []bool, w, h int) []bool {
	dilated := dilate(mask, w, h)
	return erode(dilated, w, h)
}

// dilate expands true pixels by 1 pixel (3x3 kernel).
func dilate(mask []bool, w, h int) []bool {
	out := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if !mask[y*w+x] {
				continue
			}
			for dy := -1; dy <= 1; dy++ {
				ny := y + dy
				if ny < 0 || ny >= h {
					continue
				}
				for dx := -1; dx <= 1; dx++ {
					nx := x + dx
					if nx < 0 || nx >= w {
						continue
					}
					out[ny*w+nx] = true
				}
			}
		}
	}
	return out
}

// erode shrinks true pixels by 1 pixel (3x3 kernel).
func erode(mask []bool, w, h int) []bool {
	out := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if !mask[y*w+x] {
				continue
			}
			allSet := true
			for dy := -1; dy <= 1 && allSet; dy++ {
				ny := y + dy
				if ny < 0 || ny >= h {
					allSet = false
					continue
				}
				for dx := -1; dx <= 1 && allSet; dx++ {
					nx := x + dx
					if nx < 0 || nx >= w {
						allSet = false
						continue
					}
					if !mask[ny*w+nx] {
						allSet = false
					}
				}
			}
			out[y*w+x] = allSet
		}
	}
	return out
}

// removeSmallClusters removes connected components smaller than minSize.
func removeSmallClusters(mask []bool, w, h, minSize int) {
	visited := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if !mask[idx] || visited[idx] {
				continue
			}
			// BFS to find cluster
			cluster := []int{idx}
			visited[idx] = true
			for qi := 0; qi < len(cluster); qi++ {
				ci := cluster[qi]
				cy, cx := ci/w, ci%w
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if dx == 0 && dy == 0 {
							continue
						}
						ny, nx := cy+dy, cx+dx
						if ny < 0 || ny >= h || nx < 0 || nx >= w {
							continue
						}
						ni := ny*w + nx
						if mask[ni] && !visited[ni] {
							visited[ni] = true
							cluster = append(cluster, ni)
						}
					}
				}
			}
			if len(cluster) < minSize {
				for _, ci := range cluster {
					mask[ci] = false
				}
			}
		}
	}
}

// setThickPixel draws a 2x2 pixel block.
func setThickPixel(dst *image.RGBA, x, y int, c color.RGBA) {
	bounds := dst.Bounds()
	for dy := 0; dy < 2; dy++ {
		for dx := 0; dx < 2; dx++ {
			px, py := x+dx, y+dy
			if px >= bounds.Min.X && px < bounds.Max.X && py >= bounds.Min.Y && py < bounds.Max.Y {
				dst.SetRGBA(px, py, c)
			}
		}
	}
}

// drawText1m draws "1m" as a simple bitmap at (x,y).
func drawText1m(dst *image.RGBA, x, y int, c color.RGBA) {
	// '1' pattern (3x5)
	one := [5][3]bool{
		{false, true, false},
		{true, true, false},
		{false, true, false},
		{false, true, false},
		{true, true, true},
	}
	// 'm' pattern (5x5)
	m := [5][5]bool{
		{false, false, false, false, false},
		{true, false, true, false, true},
		{true, true, true, true, true},
		{true, false, true, false, true},
		{true, false, true, false, true},
	}
	_ = math.Pi // ensure math import used

	for dy := 0; dy < 5; dy++ {
		for dx := 0; dx < 3; dx++ {
			if one[dy][dx] {
				dst.SetRGBA(x+dx, y+dy, c)
			}
		}
		for dx := 0; dx < 5; dx++ {
			if m[dy][dx] {
				dst.SetRGBA(x+4+dx, y+dy, c)
			}
		}
	}
}

// decodePGM handles simple P5 (binary) PGM format.
func decodePGM(data []byte) (image.Image, error) {
	r := bytes.NewReader(data)

	var magic string
	fmt.Fscan(r, &magic)
	if magic != "P5" {
		return nil, fmt.Errorf("not a P5 PGM file: %s", magic)
	}

	var width, height, maxVal int
	for {
		var b byte
		b, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("unexpected EOF")
		}
		if b == '#' {
			for {
				b, err = r.ReadByte()
				if err != nil || b == '\n' {
					break
				}
			}
		} else {
			r.UnreadByte()
			break
		}
	}

	_, err := fmt.Fscan(r, &width, &height, &maxVal)
	if err != nil {
		return nil, fmt.Errorf("parse PGM header: %w", err)
	}
	r.ReadByte()

	img := image.NewGray(image.Rect(0, 0, width, height))
	remaining := make([]byte, width*height)
	n, err := r.Read(remaining)
	if err != nil || n < width*height {
		return nil, fmt.Errorf("incomplete PGM data: got %d, expected %d", n, width*height)
	}
	for i := 0; i < width*height; i++ {
		img.Pix[i] = remaining[i]
	}
	return img, nil
}
