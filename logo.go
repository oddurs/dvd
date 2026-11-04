package main

import (
	"image"
	"image/color"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Classic DVD logo vector paths (viewBox: 0 0 160.5 90.5).
// Top portion (y: 0..72.3): Wikimedia public-domain DVD wordmark + elliptical disc (x: 0..151.2, center 75.6) with cutout hole.
// Bottom portion (y: 77.2..89.8): Optically kerned geometric "V I D E O" lettering centered at x=75.6 beneath the disc.
const dvdWordmarkAndDiscPath = `
M 91.2 12.7 C 91.2 12.7 80.2 26.3 80.8 27.1 C 81.6 26.3 76.8 12.5 76.8 12.5 S 75.8 9.7 72.8 0.1
H 13.8 L 11.8 8.9 H 30.8 C 40.8 8.9 46.8 13.1 45.2 20.3 C 43.4 28.3 34.8 31.7 25.4 31.7
H 22 L 26.4 12.3 H 11 L 4.4 40.5 H 26.4 C 42.8 40.5 58.6 31.7 61.4 20.3
C 61.8 18.3 61.8 12.9 60.6 9.9 C 60.4 9.5 60.2 8.9 60.6 8.9 C 60.8 8.9 61 9.1 61 9.1
L 75.2 49.9 L 110.8 8.9 H 129.4 C 139.4 8.9 145.4 13.1 143.8 20.3 C 142 28.3 133.2 31.7 124 31.7
H 120.4 L 124.8 12.5 H 109.4 L 102.8 40.7 H 124.8 C 141.2 40.7 157 31.9 159.6 20.5
C 162.2 9.1 150.8 0.3 134 0.3 H 101.2 C 92.8 10.7 91.2 12.7 91.2 12.7 Z
M 75.6 50.3 C 33.8 50.3 0 55.3 0 61.3 C 0 67.3 33.8 72.3 75.6 72.3
C 117.4 72.3 151.2 67.5 151.2 61.3 C 151.2 55.3 117.4 50.3 75.6 50.3 Z
M 72.8 65.1 C 63.2 65.1 55.6 63.5 55.6 61.5 C 55.6 59.5 63.2 57.9 72.8 57.9
C 82.4 57.9 90 59.5 90 61.5 C 90 63.5 82.4 65.1 72.8 65.1 Z
`

// Crisp geometric "V I D E O" sub-label optically kerned and centered under the disc at x=75.6 (span 24.5..126.7).
// Uses even-odd fill rule so interior counters (D and O holes) cut out cleanly.
const dvdVideoLabelPath = `
M 24.5 77.5 L 27.3 77.5 L 30.3 86.2 L 33.3 77.5 L 36.1 77.5 L 31.5 89.5 L 29.1 89.5 Z
M 48.3 77.5 H 51.0 V 89.5 H 48.3 Z
M 65.0 77.5 H 70.0 C 73.9 77.5 76.6 79.8 76.6 83.5 C 76.6 87.2 73.9 89.5 70.0 89.5 H 65.0 Z
M 67.6 79.7 V 87.3 H 69.9 C 72.4 87.3 73.9 85.8 73.9 83.5 C 73.9 81.2 72.4 79.7 69.9 79.7 Z
M 90.6 77.5 H 99.8 V 79.7 H 93.2 V 82.3 H 99.0 V 84.5 H 93.2 V 87.3 H 100.0 V 89.5 H 90.6 Z
M 120.0 77.2 C 123.9 77.2 126.7 79.8 126.7 83.5 C 126.7 87.2 123.9 89.8 120.0 89.8 C 116.1 89.8 113.3 87.2 113.3 83.5 C 113.3 79.8 116.1 77.2 120.0 77.2 Z
M 120.0 79.5 C 117.6 79.5 116.0 81.1 116.0 83.5 C 116.0 85.9 117.6 87.5 120.0 87.5 C 122.4 87.5 124.0 85.9 124.0 83.5 C 124.0 81.1 122.4 79.5 120.0 79.5 Z
`

const (
	logoViewWidth        = 160.5
	logoViewHeightVideo  = 90.5
	logoViewHeightNoText = 73.0
)

// Palette of classic vibrant DVD screensaver colors.
var dvdPalette = []color.RGBA{
	{R: 0x00, G: 0x99, B: 0xFF, A: 0xFF}, // Electric DVD Blue
	{R: 0xFF, G: 0x2A, B: 0x85, A: 0xFF}, // Hot Magenta
	{R: 0xFF, G: 0x88, B: 0x00, A: 0xFF}, // Vivid Orange
	{R: 0x00, G: 0xE6, B: 0x76, A: 0xFF}, // Neon Green
	{R: 0xB3, G: 0x66, B: 0xFF, A: 0xFF}, // Ultraviolet Purple
	{R: 0xFF, G: 0xEA, B: 0x00, A: 0xFF}, // Laser Yellow
	{R: 0x00, G: 0xE5, B: 0xFF, A: 0xFF}, // Aqua Cyan
	{R: 0xFF, G: 0x3D, B: 0x3D, A: 0xFF}, // Crimson Red
}

var cornerFlashColor = color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}

type pt struct {
	x, y float64
}

type edge struct {
	x0, y0, x1, y1 float64
}

var (
	polyOnce      sync.Once
	polysWithText [][]pt
	polysNoText   [][]pt
)

func cachedDVDPolygons(includeVideo bool) [][]pt {
	polyOnce.Do(func() {
		polysNoText = parseSVGPolygons(dvdWordmarkAndDiscPath)
		polysWithText = parseSVGPolygons(dvdWordmarkAndDiscPath + "\n" + dvdVideoLabelPath)
	})
	if includeVideo {
		return polysWithText
	}
	return polysNoText
}

// LogoMask stores a supersampled alpha mask of the DVD vector logo at target pixel dimensions.
type LogoMask struct {
	Width  int
	Height int
	Alpha  []uint8 // len = Width * Height, row-major
}

// LogoAspectRatio returns width / height for the vector logo.
func LogoAspectRatio(includeVideo bool) float64 {
	if includeVideo {
		return logoViewWidth / logoViewHeightVideo
	}
	return logoViewWidth / logoViewHeightNoText
}

// RasterizeDVDMask renders the vector DVD logo onto an anti-aliased alpha mask of size width x height
// using 8x8 supersampled scanline coverage.
func RasterizeDVDMask(width, height int, includeVideo bool) *LogoMask {
	if width < 4 {
		width = 4
	}
	if height < 2 {
		height = 2
	}

	viewH := logoViewHeightNoText
	if includeVideo {
		viewH = logoViewHeightVideo
	}
	polys := cachedDVDPolygons(includeVideo)

	// Add a 1-pixel padding margin on each side so anti-aliased outer edges never clip harshly.
	padX := 1.0
	padY := 1.0
	if width <= 16 || height <= 16 {
		padX = 0.25
		padY = 0.25
	}
	sx := (float64(width) - 2*padX) / logoViewWidth
	sy := (float64(height) - 2*padY) / viewH

	var edges []edge
	for _, poly := range polys {
		if len(poly) < 2 {
			continue
		}
		for i := 0; i < len(poly); i++ {
			a := poly[i]
			b := poly[(i+1)%len(poly)]
			x0, y0 := a.x*sx+padX, a.y*sy+padY
			x1, y1 := b.x*sx+padX, b.y*sy+padY
			if y0 == y1 {
				continue // Horizontal edges don't cross horizontal scanlines.
			}
			if y0 > y1 {
				x0, y0, x1, y1 = x1, y1, x0, y0
			}
			edges = append(edges, edge{x0: x0, y0: y0, x1: x1, y1: y1})
		}
	}

	const sub = 8 // 8x8 = 64 subpixel samples per pixel
	alpha := make([]uint8, width*height)
	counts := make([]int, width)
	var xs []float64

	for py := 0; py < height; py++ {
		clear(counts)
		for syIdx := 0; syIdx < sub; syIdx++ {
			scanY := float64(py) + (float64(syIdx)+0.5)/float64(sub)
			xs = xs[:0]
			for _, e := range edges {
				if scanY >= e.y0 && scanY < e.y1 {
					t := (scanY - e.y0) / (e.y1 - e.y0)
					xs = append(xs, e.x0+t*(e.x1-e.x0))
				}
			}
			if len(xs) < 2 {
				continue
			}
			sort.Float64s(xs)
			// Even-odd fill rule pairs intersections (0,1), (2,3), ...
			for i := 0; i+1 < len(xs); i += 2 {
				left := xs[i]
				right := xs[i+1]
				if right <= 0 || left >= float64(width) {
					continue
				}
				minPx := max(0, int(math.Floor(left)))
				maxPx := min(width, int(math.Ceil(right)))
				for px := minPx; px < maxPx; px++ {
					pxLo := float64(px)
					pxHi := float64(px + 1)
					// Fast path: full pixel span lies strictly inside [left, right).
					if pxLo >= left && pxHi <= right {
						counts[px] += sub
						continue
					}
					for sxIdx := 0; sxIdx < sub; sxIdx++ {
						sampleX := pxLo + (float64(sxIdx)+0.5)/float64(sub)
						if sampleX >= left && sampleX < right {
							counts[px]++
						}
					}
				}
			}
		}

		rowOff := py * width
		for px, c := range counts {
			if c > 0 {
				cov := float64(c) / float64(sub*sub)
				alpha[rowOff+px] = uint8(math.Round(cov * 255.0))
			}
		}
	}

	return &LogoMask{
		Width:  width,
		Height: height,
		Alpha:  alpha,
	}
}

// TintRGBA produces a non-premultiplied RGBA image (for PNG / Kitty f=100 or f=32) colored with col.
func (m *LogoMask) TintRGBA(col color.RGBA) *image.NRGBA {
	return m.TintPaddedRGBA(col, m.Width, m.Height, 0, 0)
}

// TintPaddedRGBA renders the tinted mask at (offsetX, offsetY) inside a transparent texW x texH canvas.
func (m *LogoMask) TintPaddedRGBA(col color.RGBA, texW, texH, offsetX, offsetY int) *image.NRGBA {
	if offsetX < 0 {
		offsetX = 0
	}
	if offsetY < 0 {
		offsetY = 0
	}
	if texW < m.Width+offsetX {
		texW = m.Width + offsetX
	}
	if texH < m.Height+offsetY {
		texH = m.Height + offsetY
	}
	img := image.NewNRGBA(image.Rect(0, 0, texW, texH))
	pix := img.Pix
	stride := img.Stride
	for y := 0; y < m.Height; y++ {
		srcRow := y * m.Width
		dstRow := (y+offsetY)*stride + offsetX*4
		for x := 0; x < m.Width; x++ {
			a := m.Alpha[srcRow+x]
			if a == 0 {
				continue
			}
			off := dstRow + x*4
			pix[off+0] = col.R
			pix[off+1] = col.G
			pix[off+2] = col.B
			pix[off+3] = uint8((uint16(a) * uint16(col.A)) / 255)
		}
	}
	return img
}

// Sample returns the alpha (0..255) at (x, y), or 0 out of bounds.
func (m *LogoMask) Sample(x, y int) uint8 {
	if x < 0 || x >= m.Width || y < 0 || y >= m.Height {
		return 0
	}
	return m.Alpha[y*m.Width+x]
}

// parseSVGPolygons flattens SVG path commands (M, L, H, V, C, S, Z) into closed polylines.
func parseSVGPolygons(d string) [][]pt {
	tokens := tokenizeSVGPath(d)
	var polys [][]pt
	var cur []pt
	var cx, cy float64
	var startX, startY float64
	var prevCtrlX, prevCtrlY float64
	var prevCmd byte

	idx := 0
	nextFloat := func() float64 {
		if idx >= len(tokens) {
			return 0
		}
		v, _ := strconv.ParseFloat(tokens[idx], 64)
		idx++
		return v
	}
	isCmd := func(s string) bool {
		if len(s) != 1 {
			return false
		}
		c := s[0]
		return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	}

	var cmd byte
	for idx < len(tokens) {
		if isCmd(tokens[idx]) {
			cmd = tokens[idx][0]
			idx++
		}
		switch cmd {
		case 'M', 'm':
			if len(cur) > 0 {
				polys = append(polys, cur)
				cur = nil
			}
			x, y := nextFloat(), nextFloat()
			if cmd == 'm' {
				cx += x
				cy += y
			} else {
				cx, cy = x, y
			}
			startX, startY = cx, cy
			cur = append(cur, pt{cx, cy})
			prevCmd = cmd
			if cmd == 'M' {
				cmd = 'L'
			} else {
				cmd = 'l'
			}
		case 'L', 'l':
			x, y := nextFloat(), nextFloat()
			if cmd == 'l' {
				cx += x
				cy += y
			} else {
				cx, cy = x, y
			}
			cur = append(cur, pt{cx, cy})
			prevCmd = cmd
		case 'H', 'h':
			x := nextFloat()
			if cmd == 'h' {
				cx += x
			} else {
				cx = x
			}
			cur = append(cur, pt{cx, cy})
			prevCmd = cmd
		case 'V', 'v':
			y := nextFloat()
			if cmd == 'v' {
				cy += y
			} else {
				cy = y
			}
			cur = append(cur, pt{cx, cy})
			prevCmd = cmd
		case 'C', 'c':
			x1, y1 := nextFloat(), nextFloat()
			x2, y2 := nextFloat(), nextFloat()
			x, y := nextFloat(), nextFloat()
			if cmd == 'c' {
				x1 += cx
				y1 += cy
				x2 += cx
				y2 += cy
				x += cx
				y += cy
			}
			flattenCubic(&cur, pt{cx, cy}, pt{x1, y1}, pt{x2, y2}, pt{x, y}, 24)
			prevCtrlX, prevCtrlY = x2, y2
			cx, cy = x, y
			prevCmd = cmd
		case 'S', 's':
			var x1, y1 float64
			if prevCmd == 'C' || prevCmd == 'c' || prevCmd == 'S' || prevCmd == 's' {
				x1 = 2*cx - prevCtrlX
				y1 = 2*cy - prevCtrlY
			} else {
				x1, y1 = cx, cy
			}
			x2, y2 := nextFloat(), nextFloat()
			x, y := nextFloat(), nextFloat()
			if cmd == 's' {
				x2 += cx
				y2 += cy
				x += cx
				y += cy
			}
			flattenCubic(&cur, pt{cx, cy}, pt{x1, y1}, pt{x2, y2}, pt{x, y}, 24)
			prevCtrlX, prevCtrlY = x2, y2
			cx, cy = x, y
			prevCmd = cmd
		case 'Z', 'z':
			cx, cy = startX, startY
			if len(cur) > 0 {
				polys = append(polys, cur)
				cur = nil
			}
			prevCmd = cmd
		default:
			idx++
		}
	}
	if len(cur) > 0 {
		polys = append(polys, cur)
	}
	return polys
}

func flattenCubic(out *[]pt, p0, p1, p2, p3 pt, steps int) {
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		mt := 1.0 - t
		x := mt*mt*mt*p0.x + 3*mt*mt*t*p1.x + 3*mt*t*t*p2.x + t*t*t*p3.x
		y := mt*mt*mt*p0.y + 3*mt*mt*t*p1.y + 3*mt*t*t*p2.y + t*t*t*p3.y
		*out = append(*out, pt{x, y})
	}
}

func tokenizeSVGPath(d string) []string {
	var tokens []string
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			tokens = append(tokens, buf.String())
			buf.Reset()
		}
	}
	for i := 0; i < len(d); i++ {
		ch := rune(d[i])
		switch {
		case unicode.IsSpace(ch) || ch == ',':
			flush()
		case (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z'):
			if ch == 'e' || ch == 'E' {
				buf.WriteRune(ch)
				continue
			}
			flush()
			tokens = append(tokens, string(ch))
		case ch == '-' || ch == '+':
			if buf.Len() > 0 && buf.String()[buf.Len()-1] != 'e' && buf.String()[buf.Len()-1] != 'E' {
				flush()
			}
			buf.WriteRune(ch)
		default:
			buf.WriteRune(ch)
		}
	}
	flush()
	return tokens
}

// truncateRunes truncates s to at most maxRunes Unicode code points without splitting multi-byte UTF-8 runes.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	// Fast path: every rune is >= 1 byte, so if byte length <= maxRunes it already fits.
	if len(s) <= maxRunes {
		return s
	}
	count := 0
	for idx := range s {
		if count == maxRunes {
			return s[:idx]
		}
		count++
	}
	return s
}
