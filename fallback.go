package main

import (
	"bytes"
	"image/color"
	"io"
	"math"
	"strconv"
)

// u8DecTable precomputes ASCII decimal strings for 0..255 so TrueColor SGR and CUP escapes
// format without division or strconv calls on typical terminal dimensions.
var u8DecTable = func() [256]string {
	var t [256]string
	for i := 0; i < 256; i++ {
		t[i] = strconv.Itoa(i)
	}
	return t
}()

const (
	sgrUnset   uint32 = 0xFFFFFFFF
	sgrDefault uint32 = 0xFFFFFFFE
)

func packRGB(r, g, b uint8) uint32 {
	return uint32(r)<<16 | uint32(g)<<8 | uint32(b)
}

// ansiColorLUT pre-bakes packed 24-bit RGB values and complete FG/BG TrueColor SGR escape
// byte sequences for all 256 alpha levels of a palette color so inner cell rendering
// needs zero multiplications, divisions, or multi-piece string appends.
type ansiColorLUT struct {
	col    color.RGBA
	valid  bool
	rgb    [256]uint32
	escLen [256]uint8
	fg     [256][20]byte
	bg     [256][20]byte
}

func buildColorLUT(dst *ansiColorLUT, col color.RGBA) {
	dst.col = col
	dst.valid = true
	colR, colG, colB := uint16(col.R), uint16(col.G), uint16(col.B)
	for a := 0; a < 256; a++ {
		r := uint8((colR * uint16(a)) / 255)
		g := uint8((colG * uint16(a)) / 255)
		b := uint8((colB * uint16(a)) / 255)
		dst.rgb[a] = packRGB(r, g, b)

		fb := append(dst.fg[a][:0], "\x1b[38;2;"...)
		fb = append(fb, u8DecTable[r]...)
		fb = append(fb, ';')
		fb = append(fb, u8DecTable[g]...)
		fb = append(fb, ';')
		fb = append(fb, u8DecTable[b]...)
		fb = append(fb, 'm')
		dst.escLen[a] = uint8(len(fb))

		copy(dst.bg[a][:len(fb)], fb)
		dst.bg[a][2] = '4' // "\x1b[48;2;..."
	}
}

// ANSIRenderer renders the vector DVD logo in terminals without Kitty graphics support,
// using either 4x4 sub-cell anti-aliased TrueColor Unicode half-blocks (▀/▄/█) or classic ASCII art.
type ANSIRenderer struct {
	out          io.Writer
	mask         *LogoMask
	colsW        int
	rowsH        int
	phaseCols    int
	phaseRows    int
	phaseData    []uint8
	phaseBuilt   [32]bool
	luts         [12]ansiColorLUT
	lutCount     int
	lutNext      int
	prevCol      int
	prevRow      int
	prevW        int
	prevH        int
	prevQX       int
	prevQY       int
	prevColor    color.RGBA
	prevHUD      string
	prevTermCols int
	prevTermRows int
	hasPrev      bool
	asciiOnly    bool
	blackBG      bool
	noColor      bool
	outBuf       []byte
	spaces       []byte
}

var pureASCIIArt = []string{
	` ____ __     __ ____  `,
	`|  _ \\ \   / /|  _ \ `,
	`| | | |\ \ / / | | | |`,
	`| |_| | \ V /  | |_| |`,
	`|____/   \_/   |____/ `,
	`  .----------------.  `,
	` (======( oo )======) `,
	`  '----------------'  `,
	`     V  I  D  E  O    `,
}

func NewANSIRenderer(out io.Writer, asciiOnly, blackBG, noColor bool) *ANSIRenderer {
	return &ANSIRenderer{
		out:       out,
		asciiOnly: asciiOnly,
		blackBG:   blackBG,
		noColor:   noColor,
		outBuf:    make([]byte, 0, 8192),
	}
}

func (a *ANSIRenderer) colorLUT(col color.RGBA) *ansiColorLUT {
	for i := 0; i < a.lutCount; i++ {
		if a.luts[i].col == col {
			return &a.luts[i]
		}
	}
	var slot int
	if a.lutCount < len(a.luts) {
		slot = a.lutCount
		a.lutCount++
	} else {
		slot = a.lutNext
		a.lutNext = (a.lutNext + 1) % len(a.luts)
	}
	buildColorLUT(&a.luts[slot], col)
	return &a.luts[slot]
}

func (a *ANSIRenderer) resetSGR() string {
	if a.blackBG && !a.noColor {
		return "\x1b[0m\x1b[48;2;0;0;0m"
	}
	return "\x1b[0m"
}

func (a *ANSIRenderer) baseBGState() uint32 {
	if a.blackBG && !a.noColor {
		return 0 // packed RGB(0,0,0)
	}
	return sgrDefault
}

func (a *ANSIRenderer) ensureSpaces(n int) []byte {
	if len(a.spaces) < n {
		a.spaces = bytes.Repeat([]byte{' '}, n)
	}
	return a.spaces[:n]
}

func (a *ANSIRenderer) ResizeLogo(colsW, rowsH int, includeVideo bool) {
	a.hasPrev = false
	if a.asciiOnly {
		a.colsW = len(pureASCIIArt[0])
		a.rowsH = len(pureASCIIArt)
		if !includeVideo {
			a.rowsH--
		}
		a.ensureSpaces(a.colsW + 1)
		return
	}
	if colsW < 10 {
		colsW = 10
	}
	if rowsH < 4 {
		rowsH = 4
	}
	a.colsW = colsW
	a.rowsH = rowsH
	a.ensureSpaces(colsW + 1)
	// 4 horizontal sub-samples per column x 8 vertical sub-samples per row (4x4 per half-block).
	a.mask = RasterizeDVDMask(colsW*4, rowsH*8, includeVideo)

	a.phaseCols = colsW + 1
	a.phaseRows = rowsH + 1
	needPhase := 32 * a.phaseCols * a.phaseRows * 2
	if cap(a.phaseData) < needPhase {
		a.phaseData = make([]uint8, needPhase)
	} else {
		a.phaseData = a.phaseData[:needPhase]
	}
	clear(a.phaseBuilt[:])

	needOut := (a.phaseCols*a.phaseRows)*28 + 1024
	if cap(a.outBuf) < needOut {
		a.outBuf = make([]byte, 0, needOut)
	}
}

func (a *ANSIRenderer) phaseGrid(remX, remY int) []uint8 {
	pIdx := remY*4 + remX
	stride := a.phaseCols * a.phaseRows * 2
	grid := a.phaseData[pIdx*stride : (pIdx+1)*stride]
	if !a.phaseBuilt[pIdx] {
		rowStride := a.phaseCols * 2
		for r := 0; r < a.phaseRows; r++ {
			yTop := r*8 - remY
			yBot := yTop + 4
			rOff := r * rowStride
			for c := 0; c < a.phaseCols; c++ {
				x0 := c*4 - remX
				grid[rOff+c*2] = a.mask.Sample4x4(x0, yTop)
				grid[rOff+c*2+1] = a.mask.Sample4x4(x0, yBot)
			}
		}
		a.phaseBuilt[pIdx] = true
	}
	return grid
}

func (a *ANSIRenderer) LogoCells() (w, h int) {
	return a.colsW, a.rowsH
}

func (a *ANSIRenderer) ResetPrev() {
	a.hasPrev = false
	a.prevHUD = ""
}

func (a *ANSIRenderer) ClearHUDLine(rows int) {
	a.prevHUD = ""
	if rows >= 1 {
		b := append(a.outBuf[:0], "\x1b[?2026h\x1b["...)
		b = strconv.AppendUint(b, uint64(rows), 10)
		b = append(b, ";1H"...)
		b = append(b, a.resetSGR()...)
		b = append(b, "\x1b[2K\x1b[?2026l"...)
		a.outBuf = b
		_, _ = a.out.Write(b)
	}
}

// DrawFrame renders the logo at fractional cell coordinates (colF, rowF).
// In TrueColor half-block mode, positions are quantized to quarter-columns x eighth-rows
// using cached 4x4 sub-cell phase grids, pre-baked SGR LUTs, cross-row SGR deduplication, and differential trail erasing.
func (a *ANSIRenderer) DrawFrame(colF, rowF float64, termCols, termRows int, col color.RGBA, hud string) error {
	if termCols < 1 || termRows < 1 {
		return nil
	}
	if colF < 0 {
		colF = 0
	}
	if rowF < 0 {
		rowF = 0
	}

	var qx, qy, col0, row0, remX, remY, spanCols, spanRows int
	if a.asciiOnly {
		col0 = int(math.Round(colF))
		row0 = int(math.Round(rowF))
		qx, qy = col0, row0
		spanCols = a.colsW
		spanRows = a.rowsH
	} else {
		qx = int(math.Round(colF * 4.0))
		qy = int(math.Round(rowF * 8.0))
		col0 = qx / 4
		remX = qx & 3
		row0 = qy / 8
		remY = qy & 7
		spanCols = a.colsW
		if remX > 0 {
			spanCols++
		}
		spanRows = a.rowsH
		if remY > 0 {
			spanRows++
		}
	}

	if hud != "" && termRows >= 2 && termCols > 4 {
		hud = truncateRunes(hud, termCols-2)
	} else {
		hud = ""
	}

	if a.hasPrev && qx == a.prevQX && qy == a.prevQY &&
		col == a.prevColor && hud == a.prevHUD &&
		termCols == a.prevTermCols && termRows == a.prevTermRows {
		return nil
	}

	rst := a.resetSGR()
	baseBG := a.baseBGState()
	b := append(a.outBuf[:0], "\x1b[?2026h"...)
	curFG := sgrUnset
	curBG := sgrUnset

	// Differential erase: clear only cells of the previous box that lie outside the new box.
	if a.hasPrev {
		pL := max(0, a.prevCol)
		pR := min(termCols, a.prevCol+a.prevW)
		cL := max(0, col0)
		cR := min(termCols, col0+spanCols)
		if pR > pL {
			for pr := a.prevRow; pr < a.prevRow+a.prevH; pr++ {
				ry := pr + 1
				if ry < 1 || ry > termRows {
					continue
				}
				if pr < row0 || pr >= row0+spanRows || cL >= pR || cR <= pL {
					b = appendCup(b, ry, pL+1)
					if curBG != baseBG {
						b = append(b, rst...)
						curFG = sgrDefault
						curBG = baseBG
					}
					b = append(b, a.ensureSpaces(pR-pL)...)
					continue
				}
				if pL < cL {
					w := min(pR, cL) - pL
					b = appendCup(b, ry, pL+1)
					if curBG != baseBG {
						b = append(b, rst...)
						curFG = sgrDefault
						curBG = baseBG
					}
					b = append(b, a.ensureSpaces(w)...)
				}
				if pR > cR {
					start := max(pL, cR)
					w := pR - start
					b = appendCup(b, ry, start+1)
					if curBG != baseBG {
						b = append(b, rst...)
						curFG = sgrDefault
						curBG = baseBG
					}
					b = append(b, a.ensureSpaces(w)...)
				}
			}
		}
	}

	if a.asciiOnly {
		if a.noColor {
			b = append(b, "\x1b[1m"...)
		} else {
			if a.blackBG {
				b = append(b, "\x1b[48;2;0;0;0m"...)
			}
			b = append(b, "\x1b[1;38;2;"...)
			b = appendRGB(b, col.R, col.G, col.B)
			b = append(b, 'm')
		}
		for r := 0; r < a.rowsH && r < len(pureASCIIArt); r++ {
			ry := row0 + r + 1
			if ry < 1 || ry > termRows {
				continue
			}
			line := pureASCIIArt[r]
			maxLen := termCols - col0
			if maxLen <= 0 {
				continue
			}
			if len(line) > maxLen {
				line = line[:maxLen]
			}
			b = appendCup(b, ry, col0+1)
			b = append(b, line...)
		}
		b = append(b, rst...)
		curFG = sgrDefault
		curBG = baseBG
	} else if a.mask != nil {
		grid := a.phaseGrid(remX, remY)
		rowStride := a.phaseCols * 2
		maxCols := min(spanCols, termCols-col0)
		var lut *ansiColorLUT
		if !a.noColor {
			lut = a.colorLUT(col)
		} else if curFG == sgrUnset {
			b = append(b, rst...)
			curFG = sgrDefault
			curBG = baseBG
		}
		for r := 0; r < spanRows; r++ {
			ry := row0 + r + 1
			if ry < 1 || ry > termRows {
				continue
			}
			b = appendCup(b, ry, col0+1)
			rOff := r * rowStride
			for c := 0; c < maxCols; c++ {
				topA := grid[rOff+c*2]
				botA := grid[rOff+c*2+1]
				if topA < 10 && botA < 10 {
					if !a.noColor && curBG != baseBG {
						b = append(b, rst...)
						curFG = sgrDefault
						curBG = baseBG
					}
					b = append(b, ' ')
					continue
				}
				if a.noColor {
					if topA >= 64 && botA < 64 {
						b = append(b, "▀"...)
					} else if topA < 64 && botA >= 64 {
						b = append(b, "▄"...)
					} else if topA >= 64 && botA >= 64 {
						b = append(b, "█"...)
					} else {
						b = append(b, ' ')
					}
					continue
				}
				if topA >= 10 && botA < 10 {
					wantFG := lut.rgb[topA]
					if curBG != baseBG {
						b = append(b, rst...)
						curBG = baseBG
						curFG = sgrDefault
					}
					if curFG != wantFG {
						b = append(b, lut.fg[topA][:lut.escLen[topA]]...)
						curFG = wantFG
					}
					b = append(b, "▀"...)
				} else if topA < 10 && botA >= 10 {
					wantFG := lut.rgb[botA]
					if curBG != baseBG {
						b = append(b, rst...)
						curBG = baseBG
						curFG = sgrDefault
					}
					if curFG != wantFG {
						b = append(b, lut.fg[botA][:lut.escLen[botA]]...)
						curFG = wantFG
					}
					b = append(b, "▄"...)
				} else {
					wantFG := lut.rgb[topA]
					wantBG := lut.rgb[botA]
					if wantFG == wantBG {
						if curFG != wantFG {
							b = append(b, lut.fg[topA][:lut.escLen[topA]]...)
							curFG = wantFG
						}
						b = append(b, "█"...)
						continue
					}
					if curFG != wantFG {
						b = append(b, lut.fg[topA][:lut.escLen[topA]]...)
						curFG = wantFG
					}
					if curBG != wantBG {
						b = append(b, lut.bg[botA][:lut.escLen[botA]]...)
						curBG = wantBG
					}
					b = append(b, "▀"...)
				}
			}
		}
		if !a.noColor && (curFG != sgrDefault || curBG != baseBG) {
			b = append(b, rst...)
			curFG = sgrDefault
			curBG = baseBG
		}
	}

	if hud != "" {
		b = appendCup(b, termRows, 1)
		if curFG != sgrDefault || curBG != baseBG {
			b = append(b, rst...)
		}
		if !a.noColor {
			b = append(b, "\x1b[2K\x1b[38;5;242m "...)
		} else {
			b = append(b, "\x1b[2K "...)
		}
		b = append(b, hud...)
		b = append(b, rst...)
	}

	b = append(b, "\x1b[?2026l"...)
	a.outBuf = b

	a.prevCol = col0
	a.prevRow = row0
	a.prevW = spanCols
	a.prevH = spanRows
	a.prevQX = qx
	a.prevQY = qy
	a.prevColor = col
	a.prevHUD = hud
	a.prevTermCols = termCols
	a.prevTermRows = termRows
	a.hasPrev = true

	_, err := a.out.Write(b)
	return err
}

func appendCup(b []byte, row1, col1 int) []byte {
	b = append(b, "\x1b["...)
	if uint(row1) < 256 {
		b = append(b, u8DecTable[row1]...)
	} else {
		b = strconv.AppendUint(b, uint64(row1), 10)
	}
	b = append(b, ';')
	if uint(col1) < 256 {
		b = append(b, u8DecTable[col1]...)
	} else {
		b = strconv.AppendUint(b, uint64(col1), 10)
	}
	return append(b, 'H')
}

func appendRGB(b []byte, r, g, bl uint8) []byte {
	b = append(b, u8DecTable[r]...)
	b = append(b, ';')
	b = append(b, u8DecTable[g]...)
	b = append(b, ';')
	return append(b, u8DecTable[bl]...)
}
