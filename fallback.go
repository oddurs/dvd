package main

import (
	"bytes"
	"fmt"
	"image/color"
	"io"
)

// ANSIRenderer renders the vector DVD logo in terminals without Kitty graphics support,
// using either TrueColor Unicode half-blocks (▀/▄/█) or classic retro ASCII art.
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
	buf       bytes.Buffer
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

func NewANSIRenderer(out io.Writer, asciiOnly bool) *ANSIRenderer {
	return &ANSIRenderer{
		out:       out,
		asciiOnly: asciiOnly,
	}
}

func (a *ANSIRenderer) ResizeLogo(colsW, rowsH int, includeVideo bool) {
	if a.asciiOnly {
		a.colsW = len(pureASCIIArt[0])
		a.rowsH = len(pureASCIIArt)
		if !includeVideo {
			a.rowsH -= 1
		}
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
	// Each terminal row encodes 2 vertical subpixels via half-block characters.
	a.mask = RasterizeDVDMask(colsW, rowsH*2, includeVideo)
}

func (a *ANSIRenderer) LogoCells() (w, h int) {
	return a.colsW, a.rowsH
}

func (a *ANSIRenderer) ResetPrev() {
	a.hasPrev = false
}

func (a *ANSIRenderer) DrawFrame(col0, row0, termCols, termRows int, col color.RGBA, hud string) error {
	if col0 < 0 {
		col0 = 0
	}
	if row0 < 0 {
		row0 = 0
	}

	a.buf.Reset()
	a.buf.WriteString("\x1b[?2026h")

	// Erase previous bounding box so no trails remain.
	if a.hasPrev {
		for r := 0; r < a.prevH; r++ {
			ry := a.prevRow + r + 1
			if ry < 1 || ry > termRows {
				continue
			}
			cx := a.prevCol + 1
			if cx < 1 {
				cx = 1
			}
			w := a.prevW
			if cx+w-1 > termCols {
				w = termCols - cx + 1
			}
			if w > 0 {
				fmt.Fprintf(&a.buf, "\x1b[%d;%dH\x1b[0m%*s", ry, cx, w, "")
			}
		}
	}

	if a.asciiOnly {
		fmt.Fprintf(&a.buf, "\x1b[1;38;2;%d;%d;%dm", col.R, col.G, col.B)
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
			fmt.Fprintf(&a.buf, "\x1b[%d;%dH%s", ry, col0+1, line)
		}
		a.buf.WriteString("\x1b[0m")
	} else if a.mask != nil {
		for r := 0; r < a.rowsH; r++ {
			ry := row0 + r + 1
			if ry < 1 || ry > termRows {
				continue
			}
			fmt.Fprintf(&a.buf, "\x1b[%d;%dH", ry, col0+1)
			for c := 0; c < a.colsW; c++ {
				if col0+c >= termCols {
					break
				}
				topA := a.mask.Sample(c, r*2)
				botA := a.mask.Sample(c, r*2+1)
				if topA < 32 && botA < 32 {
					a.buf.WriteString("\x1b[0m ")
					continue
				}
				tr := (int(col.R) * int(topA)) / 255
				tg := (int(col.G) * int(topA)) / 255
				tb := (int(col.B) * int(topA)) / 255
				br := (int(col.R) * int(botA)) / 255
				bg := (int(col.G) * int(botA)) / 255
				bb := (int(col.B) * int(botA)) / 255
				if topA >= 32 && botA < 32 {
					fmt.Fprintf(&a.buf, "\x1b[0m\x1b[38;2;%d;%d;%dm▀", tr, tg, tb)
				} else if topA < 32 && botA >= 32 {
					fmt.Fprintf(&a.buf, "\x1b[0m\x1b[38;2;%d;%d;%dm▄", br, bg, bb)
				} else {
					fmt.Fprintf(&a.buf, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀", tr, tg, tb, br, bg, bb)
				}
			}
			a.buf.WriteString("\x1b[0m")
		}
	}

	if hud != "" && termRows >= 2 {
		if len(hud) > termCols-2 && termCols > 4 {
			hud = hud[:termCols-2]
		}
		fmt.Fprintf(&a.buf, "\x1b[%d;1H\x1b[2K\x1b[38;5;242m %s\x1b[0m", termRows, hud)
	}

	a.buf.WriteString("\x1b[?2026l")

	a.prevCol = col0
	a.prevRow = row0
	a.prevW = a.colsW
	a.prevH = a.rowsH
	a.hasPrev = true

	_, err := a.out.Write(a.buf.Bytes())
	return err
}
