package main

import (
	"bytes"
	"image/color"
	"io"
	"math"
)

// ANSIRenderer renders the vector DVD logo in terminals without Kitty graphics support,
// using either 4x4 sub-cell anti-aliased TrueColor Unicode half-blocks (▀/▄) or classic ASCII art.
type ANSIRenderer struct {
	out          io.Writer
	mask         *LogoMask
	colsW        int
	rowsH        int
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
	buf          bytes.Buffer
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
	}
}

func (a *ANSIRenderer) resetSGR() string {
	if a.blackBG && !a.noColor {
		return "\x1b[0m\x1b[48;2;0;0;0m"
	}
	return "\x1b[0m"
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
		a.buf.Reset()
		a.buf.WriteString("\x1b[?2026h\x1b[")
		writeUint(&a.buf, uint64(rows))
		a.buf.WriteString(";1H")
		a.buf.WriteString(a.resetSGR())
		a.buf.WriteString("\x1b[2K\x1b[?2026l")
		_, _ = a.out.Write(a.buf.Bytes())
	}
}

// DrawFrame renders the logo at fractional cell coordinates (colF, rowF).
// In TrueColor half-block mode, positions are quantized to quarter-columns x eighth-rows
// using 4x4 area-integrated sub-cell anti-aliasing and differential trail erasing.
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
	a.buf.Reset()
	a.buf.WriteString("\x1b[?2026h")

	// Differential erase: clear only cells of the previous box that lie outside the new box,
	// avoiding blank-flash shimmer even in terminals without ?2026h synchronized output.
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
					writeCup(&a.buf, ry, pL+1)
					a.buf.WriteString(rst)
					a.buf.Write(a.ensureSpaces(pR - pL))
					continue
				}
				if pL < cL {
					w := min(pR, cL) - pL
					writeCup(&a.buf, ry, pL+1)
					a.buf.WriteString(rst)
					a.buf.Write(a.ensureSpaces(w))
				}
				if pR > cR {
					start := max(pL, cR)
					w := pR - start
					writeCup(&a.buf, ry, start+1)
					a.buf.WriteString(rst)
					a.buf.Write(a.ensureSpaces(w))
				}
			}
		}
	}

	if a.asciiOnly {
		if a.noColor {
			a.buf.WriteString("\x1b[1m")
		} else {
			if a.blackBG {
				a.buf.WriteString("\x1b[48;2;0;0;0m")
			}
			a.buf.WriteString("\x1b[1;38;2;")
			writeRGB(&a.buf, col.R, col.G, col.B)
			a.buf.WriteByte('m')
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
			writeCup(&a.buf, ry, col0+1)
			a.buf.WriteString(line)
		}
		a.buf.WriteString(rst)
	} else if a.mask != nil {
		for r := 0; r < spanRows; r++ {
			ry := row0 + r + 1
			if ry < 1 || ry > termRows {
				continue
			}
			writeCup(&a.buf, ry, col0+1)
			yTop := r*8 - remY
			yBot := yTop + 4
			for c := 0; c < spanCols; c++ {
				if col0+c >= termCols {
					break
				}
				x0 := c*4 - remX
				topA := a.mask.Sample4x4(x0, yTop)
				botA := a.mask.Sample4x4(x0, yBot)
				if topA < 10 && botA < 10 {
					a.buf.WriteString(rst)
					a.buf.WriteByte(' ')
					continue
				}
				if a.noColor {
					if topA >= 64 && botA < 64 {
						a.buf.WriteString("▀")
					} else if topA < 64 && botA >= 64 {
						a.buf.WriteString("▄")
					} else if topA >= 64 && botA >= 64 {
						a.buf.WriteString("█")
					} else {
						a.buf.WriteByte(' ')
					}
					continue
				}
				tr := uint8((uint16(col.R) * uint16(topA)) / 255)
				tg := uint8((uint16(col.G) * uint16(topA)) / 255)
				tb := uint8((uint16(col.B) * uint16(topA)) / 255)
				br := uint8((uint16(col.R) * uint16(botA)) / 255)
				bg := uint8((uint16(col.G) * uint16(botA)) / 255)
				bb := uint8((uint16(col.B) * uint16(botA)) / 255)
				if topA >= 10 && botA < 10 {
					a.buf.WriteString(rst)
					a.buf.WriteString("\x1b[38;2;")
					writeRGB(&a.buf, tr, tg, tb)
					a.buf.WriteString("m▀")
				} else if topA < 10 && botA >= 10 {
					a.buf.WriteString(rst)
					a.buf.WriteString("\x1b[38;2;")
					writeRGB(&a.buf, br, bg, bb)
					a.buf.WriteString("m▄")
				} else {
					a.buf.WriteString("\x1b[38;2;")
					writeRGB(&a.buf, tr, tg, tb)
					a.buf.WriteString("m\x1b[48;2;")
					writeRGB(&a.buf, br, bg, bb)
					a.buf.WriteString("m▀")
				}
			}
			a.buf.WriteString(rst)
		}
	}

	if hud != "" {
		writeCup(&a.buf, termRows, 1)
		a.buf.WriteString(rst)
		if !a.noColor {
			a.buf.WriteString("\x1b[2K\x1b[38;5;242m ")
		} else {
			a.buf.WriteString("\x1b[2K ")
		}
		a.buf.WriteString(hud)
		a.buf.WriteString(rst)
	}

	a.buf.WriteString("\x1b[?2026l")

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

	_, err := a.out.Write(a.buf.Bytes())
	return err
}

func writeCup(dst *bytes.Buffer, row1, col1 int) {
	dst.WriteString("\x1b[")
	writeUint(dst, uint64(row1))
	dst.WriteByte(';')
	writeUint(dst, uint64(col1))
	dst.WriteByte('H')
}

func writeRGB(dst *bytes.Buffer, r, g, b uint8) {
	writeUint(dst, uint64(r))
	dst.WriteByte(';')
	writeUint(dst, uint64(g))
	dst.WriteByte(';')
	writeUint(dst, uint64(b))
}
