package main

import (
	"bytes"
	"image/color"
	"io"
)

// ANSIRenderer renders the vector DVD logo in terminals without Kitty graphics support,
// using either sub-row TrueColor Unicode half-blocks (▀/▄) or classic retro ASCII art.
type ANSIRenderer struct {
	out       io.Writer
	mask      *LogoMask
	colsW     int
	rowsH     int
	prevCol   int
	prevRow   int
	prevW     int
	prevH     int
	hasPrev   bool
	asciiOnly bool
	blackBG   bool
	noColor   bool
	buf       bytes.Buffer
	spaces    []byte
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
	if a.asciiOnly {
		a.colsW = len(pureASCIIArt[0])
		a.rowsH = len(pureASCIIArt)
		if !includeVideo {
			a.rowsH--
		}
		a.ensureSpaces(a.colsW)
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
	a.ensureSpaces(colsW)
	// Each terminal row encodes 2 vertical subpixels via half-block characters.
	a.mask = RasterizeDVDMask(colsW, rowsH*2, includeVideo)
}

func (a *ANSIRenderer) LogoCells() (w, h int) {
	return a.colsW, a.rowsH
}

func (a *ANSIRenderer) ResetPrev() {
	a.hasPrev = false
}

func (a *ANSIRenderer) ClearHUDLine(rows int) {
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

// DrawFrame renders the logo at cell (col0, row0) with optional half-row vertical phase
// oddHalf in {0, 1} so half-block mode steps smoothly by 0.5-row increments.
func (a *ANSIRenderer) DrawFrame(col0, row0, oddHalf, termCols, termRows int, col color.RGBA, hud string) error {
	if termCols < 1 || termRows < 1 {
		return nil
	}
	if col0 < 0 {
		col0 = 0
	}
	if row0 < 0 {
		row0 = 0
	}
	if a.asciiOnly || oddHalf <= 0 {
		oddHalf = 0
	} else {
		oddHalf = 1
	}

	rst := a.resetSGR()
	a.buf.Reset()
	a.buf.WriteString("\x1b[?2026h")

	// Erase previous bounding box so no trails remain.
	if a.hasPrev {
		for r := 0; r < a.prevH; r++ {
			ry := a.prevRow + r + 1
			if ry < 1 || ry > termRows {
				continue
			}
			cx := max(1, a.prevCol+1)
			w := a.prevW
			if cx+w-1 > termCols {
				w = termCols - cx + 1
			}
			if w > 0 {
				writeCup(&a.buf, ry, cx)
				a.buf.WriteString(rst)
				a.buf.Write(a.ensureSpaces(w))
			}
		}
	}

	spanRows := a.rowsH
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
		spanRows = a.rowsH + oddHalf
		for r := 0; r < spanRows; r++ {
			ry := row0 + r + 1
			if ry < 1 || ry > termRows {
				continue
			}
			writeCup(&a.buf, ry, col0+1)
			for c := 0; c < a.colsW; c++ {
				if col0+c >= termCols {
					break
				}
				topA := a.mask.Sample(c, r*2-oddHalf)
				botA := a.mask.Sample(c, r*2+1-oddHalf)
				if topA < 32 && botA < 32 {
					a.buf.WriteString(rst)
					a.buf.WriteByte(' ')
					continue
				}
				if a.noColor {
					if topA >= 32 && botA < 32 {
						a.buf.WriteString("▀")
					} else if topA < 32 && botA >= 32 {
						a.buf.WriteString("▄")
					} else {
						a.buf.WriteString("█")
					}
					continue
				}
				tr := uint8((uint16(col.R) * uint16(topA)) / 255)
				tg := uint8((uint16(col.G) * uint16(topA)) / 255)
				tb := uint8((uint16(col.B) * uint16(topA)) / 255)
				br := uint8((uint16(col.R) * uint16(botA)) / 255)
				bg := uint8((uint16(col.G) * uint16(botA)) / 255)
				bb := uint8((uint16(col.B) * uint16(botA)) / 255)
				if topA >= 32 && botA < 32 {
					a.buf.WriteString(rst)
					a.buf.WriteString("\x1b[38;2;")
					writeRGB(&a.buf, tr, tg, tb)
					a.buf.WriteString("m▀")
				} else if topA < 32 && botA >= 32 {
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

	if hud != "" && termRows >= 2 && termCols > 4 {
		hud = truncateRunes(hud, termCols-2)
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
	a.prevW = a.colsW
	a.prevH = spanRows
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
