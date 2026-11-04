package main

import (
	"image"
	"image/color"
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
	rasterSubSamples     = 8
	rasterMaxCoverage    = rasterSubSamples * rasterSubSamples // 64
)

// covToAlpha maps 8x8 subpixel coverage counts (0..64) directly to 0..255 alpha without float division.
var covToAlpha = func() [rasterMaxCoverage + 1]uint8 {
	var lut [rasterMaxCoverage + 1]uint8
	for c := 0; c <= rasterMaxCoverage; c++ {
		lut[c] = uint8((c*255 + rasterMaxCoverage/2) / rasterMaxCoverage)
	}
	return lut
}()

// identityCovLUT preserves raw 0..64 subpixel coverage indices for 1-byte/px indexed PNG encoding.
var identityCovLUT = func() [rasterMaxCoverage + 1]uint8 {
	var lut [rasterMaxCoverage + 1]uint8
	for c := 0; c <= rasterMaxCoverage; c++ {
		lut[c] = uint8(c)
	}
	return lut
}()

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
	x0, y0, y1, slope float64
}

type rasterScratch struct {
	edges      []edge
	edgeNext   []int32
	bucketHead []int32
	active     []edge
	counts     []uint8
	fullDelta  []int16
	xs         []float64
}

var scratchPool = sync.Pool{
	New: func() any {
		return &rasterScratch{
			edges:      make([]edge, 0, 320),
			edgeNext:   make([]int32, 0, 320),
			bucketHead: make([]int32, 0, 512),
			active:     make([]edge, 0, 32),
			counts:     make([]uint8, 0, 1024),
			fullDelta:  make([]int16, 0, 1025),
			xs:         make([]float64, 0, 32),
		}
	},
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

// LogoMask stores a supersampled alpha mask of the DVD vector logo.
type LogoMask struct {
	Width  int
	Height int
	Alpha  []uint8 // len = Width * Height, 0..255
}

// LogoAspectRatio returns width / height for the vector logo.
func LogoAspectRatio(includeVideo bool) float64 {
	if includeVideo {
		return logoViewWidth / logoViewHeightVideo
	}
	return logoViewWidth / logoViewHeightNoText
}

// sortFloats sorts small intersection slices in-place with zero heap allocations.
func sortFloats(xs []float64) {
	for i := 1; i < len(xs); i++ {
		v := xs[i]
		j := i - 1
		for j >= 0 && xs[j] > v {
			xs[j+1] = xs[j]
			j--
		}
		xs[j+1] = v
	}
}

// RasterizeDVDMask renders the vector DVD logo onto an anti-aliased alpha mask of size width x height
// using 8x8 supersampled scanline coverage with a Y-bucketed Active Edge Table and O(1) span integration.
func RasterizeDVDMask(width, height int, includeVideo bool) *LogoMask {
	if width < 4 {
		width = 4
	}
	if height < 2 {
		height = 2
	}
	alpha := make([]uint8, width*height)
	rasterizeDVDInto(alpha, width, 0, 0, width, height, includeVideo, &covToAlpha, nil)
	return &LogoMask{
		Width:  width,
		Height: height,
		Alpha:  alpha,
	}
}

// rasterizeDVDInto renders the vector DVD logo of size width x height directly into dst at
// (offX, offY) with row stride `stride`, mapping non-zero 0..64 coverage counts through lut.
// Caller must ensure dst in the target rectangle is zero-initialized.
func rasterizeDVDInto(
	dst []uint8,
	stride, offX, offY, width, height int,
	includeVideo bool,
	lut *[rasterMaxCoverage + 1]uint8,
	sc *rasterScratch,
) {
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

	pooled := false
	if sc == nil {
		sc = scratchPool.Get().(*rasterScratch)
		pooled = true
	}
	if cap(sc.bucketHead) < height {
		sc.bucketHead = make([]int32, height)
	}
	bucketHead := sc.bucketHead[:height]
	for i := range bucketHead {
		bucketHead[i] = -1
	}

	edges := sc.edges[:0]
	edgeNext := sc.edgeNext[:0]
	fHeight := float64(height)

	for _, poly := range polys {
		if len(poly) < 2 {
			continue
		}
		a := poly[len(poly)-1]
		x0, y0 := a.x*sx+padX, a.y*sy+padY
		for i := 0; i < len(poly); i++ {
			b := poly[i]
			x1, y1 := b.x*sx+padX, b.y*sy+padY
			if y0 != y1 {
				ex0, ey0, ex1, ey1 := x0, y0, x1, y1
				if ey0 > ey1 {
					ex0, ey0, ex1, ey1 = ex1, ey1, ex0, ey0
				}
				if ey1 > 0 && ey0 < fHeight {
					startRow := 0
					if ey0 > 0 {
						startRow = int(ey0)
					}
					if startRow < height {
						idx := int32(len(edges))
						edges = append(edges, edge{
							x0:    ex0,
							y0:    ey0,
							y1:    ey1,
							slope: (ex1 - ex0) / (ey1 - ey0),
						})
						edgeNext = append(edgeNext, bucketHead[startRow])
						bucketHead[startRow] = idx
					}
				}
			}
			x0, y0 = x1, y1
		}
	}

	if cap(sc.counts) < width {
		sc.counts = make([]uint8, width)
	}
	counts := sc.counts[:width]
	if cap(sc.fullDelta) < width+1 {
		sc.fullDelta = make([]int16, width+1)
	}
	fullDelta := sc.fullDelta[:width+1]
	active := sc.active[:0]
	xs := sc.xs[:0]

	maxSubX := width * rasterSubSamples
	fWidth := float64(width)

	for py := 0; py < height; py++ {
		for eIdx := bucketHead[py]; eIdx >= 0; eIdx = edgeNext[eIdx] {
			active = append(active, edges[eIdx])
		}
		if len(active) == 0 {
			continue
		}

		pyLo := float64(py)
		keep := 0
		for _, e := range active {
			if e.y1 > pyLo {
				active[keep] = e
				keep++
			}
		}
		active = active[:keep]
		if len(active) < 2 {
			continue
		}

		clear(counts)
		clear(fullDelta)
		hasSpan := false
		for syIdx := 0; syIdx < rasterSubSamples; syIdx++ {
			scanY := pyLo + (float64(syIdx)+0.5)*(1.0/rasterSubSamples)
			xs = xs[:0]
			for _, e := range active {
				if scanY >= e.y0 && scanY < e.y1 {
					xs = append(xs, e.x0+(scanY-e.y0)*e.slope)
				}
			}
			if len(xs) < 2 {
				continue
			}
			sortFloats(xs)
			// Even-odd fill rule pairs intersections (0,1), (2,3), ...
			for i := 0; i+1 < len(xs); i += 2 {
				left := xs[i]
				right := xs[i+1]
				if right <= 0 || left >= fWidth {
					continue
				}
				if left < 0 {
					left = 0
				}
				if right > fWidth {
					right = fWidth
				}

				// Subpixel sample j (0..maxSubX-1) has center (j + 0.5)/8 in [left, right)
				// iff ceil(left*8 - 0.5) <= j < ceil(right*8 - 0.5).
				uLo := left*rasterSubSamples - 0.5
				jLo := int(uLo)
				if float64(jLo) < uLo {
					jLo++
				}
				uHi := right*rasterSubSamples - 0.5
				jHi := int(uHi)
				if float64(jHi) < uHi {
					jHi++
				}
				if jLo < 0 {
					jLo = 0
				}
				if jHi > maxSubX {
					jHi = maxSubX
				}
				if jHi <= jLo {
					continue
				}

				hasSpan = true
				px0 := jLo >> 3
				px1 := jHi >> 3
				if px0 == px1 {
					counts[px0] += uint8(jHi - jLo)
					continue
				}
				counts[px0] += uint8(rasterSubSamples - (jLo & 7))
				if px0+1 < px1 {
					fullDelta[px0+1] += rasterSubSamples
					fullDelta[px1] -= rasterSubSamples
				}
				if rem1 := jHi & 7; rem1 != 0 {
					counts[px1] += uint8(rem1)
				}
			}
		}
		if !hasSpan {
			continue
		}

		dstRow := dst[(py+offY)*stride+offX : (py+offY)*stride+offX+width]
		var fullAcc int16
		for px, c := range counts {
			fullAcc += fullDelta[px]
			total := uint8(fullAcc) + c
			if total != 0 {
				dstRow[px] = lut[total]
			}
		}
	}

	sc.edges = edges[:0]
	sc.edgeNext = edgeNext[:0]
	sc.active = active[:0]
	sc.xs = xs[:0]
	if pooled {
		scratchPool.Put(sc)
	}
}

// TintRGBA produces a non-premultiplied RGBA image colored with col.
func (m *LogoMask) TintRGBA(col color.RGBA) *image.NRGBA {
	return m.TintPaddedRGBA(col, m.Width, m.Height, 0, 0)
}

// TintPaddedRGBA renders the tinted mask at (offsetX, offsetY) inside a transparent texW x texH canvas.
// All pixels (including alpha=0 margins) carry (col.R, col.G, col.B) so GPU hardware bilinear
// filtering never bleeds dark fringes at sub-pixel offsets and PNG row predictors see constant RGB.
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

	if len(pix) >= 4 {
		for x := 0; x < texW; x++ {
			off := x * 4
			pix[off+0] = col.R
			pix[off+1] = col.G
			pix[off+2] = col.B
			pix[off+3] = 0
		}
		for filled := stride; filled < len(pix); filled *= 2 {
			copy(pix[filled:], pix[:filled])
		}
	}

	colA := uint16(col.A)
	for y := 0; y < m.Height; y++ {
		srcRow := y * m.Width
		dstRow := (y+offsetY)*stride + offsetX*4
		for x := 0; x < m.Width; x++ {
			a := m.Alpha[srcRow+x]
			if a == 0 {
				continue
			}
			pix[dstRow+x*4+3] = uint8((uint16(a) * colA) / 255)
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

// Sample4x4 returns the area-averaged alpha (0..255) of the 4x4 texel block [x0..x0+4, y0..y0+4).
func (m *LogoMask) Sample4x4(x0, y0 int) uint8 {
	if x0+4 <= 0 || x0 >= m.Width || y0+4 <= 0 || y0 >= m.Height {
		return 0
	}
	var sum uint32
	if x0 >= 0 && x0+4 <= m.Width && y0 >= 0 && y0+4 <= m.Height {
		w := m.Width
		r0 := y0*w + x0
		r1 := r0 + w
		r2 := r1 + w
		r3 := r2 + w
		a := m.Alpha
		sum = uint32(a[r0]) + uint32(a[r0+1]) + uint32(a[r0+2]) + uint32(a[r0+3]) +
			uint32(a[r1]) + uint32(a[r1+1]) + uint32(a[r1+2]) + uint32(a[r1+3]) +
			uint32(a[r2]) + uint32(a[r2+1]) + uint32(a[r2+2]) + uint32(a[r2+3]) +
			uint32(a[r3]) + uint32(a[r3+1]) + uint32(a[r3+2]) + uint32(a[r3+3])
		return uint8((sum + 8) >> 4)
	}
	for dy := 0; dy < 4; dy++ {
		yy := y0 + dy
		if yy < 0 || yy >= m.Height {
			continue
		}
		row := yy * m.Width
		for dx := 0; dx < 4; dx++ {
			xx := x0 + dx
			if xx >= 0 && xx < m.Width {
				sum += uint32(m.Alpha[row+xx])
			}
		}
	}
	return uint8((sum + 8) >> 4)
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
