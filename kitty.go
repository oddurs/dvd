package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strconv"
)

const kittyPlacementID = 1

// KittyRenderer manages GPU-cached logo textures and sub-cell smooth placements
// using the Kitty Graphics Protocol.
//
// Instead of relying on X=/Y= cell offsets (which cause horizontal cell-span rounding
// "jelly" stretching in some renderers or snap jumps when reported cell pixel sizes
// differ by DPI scale), each texture is padded with a transparent margin and displayed
// into a FIXED (c x r) cell box via source-rectangle cropping (x, y, w, h).
// That keeps the rendered cell box 100% constant while sliding smoothly pixel-by-pixel
// across cell boundaries.
type KittyRenderer struct {
	out          io.Writer
	nextBaseID   uint32
	colorIDs     []uint32
	flashID      uint32
	activeImgID  uint32
	logoW, logoH int
	cellW, cellH int
	placeCols    int
	placeRows    int
	cropW, cropH int
	marginX      int
	marginY      int
	buf          bytes.Buffer
}

func NewKittyRenderer(out io.Writer) *KittyRenderer {
	return &KittyRenderer{
		out:        out,
		nextBaseID: 100,
	}
}

// UploadPalette rasterizes the vector DVD logo at (logoW x logoH) pixels inside a padded
// transparent atlas aligned to (cellW, cellH), and uploads each palette color + flash frame.
func (k *KittyRenderer) UploadPalette(
	logoW, logoH, cellW, cellH, termCols, termRows int,
	includeVideo bool,
	palette []color.RGBA,
	flash color.RGBA,
) error {
	if cellW < 1 {
		cellW = 16
	}
	if cellH < 1 {
		cellH = 36
	}
	if termCols < 2 {
		termCols = 2
	}
	if termRows < 2 {
		termRows = 2
	}
	if maxW := (termCols - 1) * cellW; logoW > maxW && maxW >= 8 {
		logoW = maxW
	}
	if maxH := (termRows - 1) * cellH; logoH > maxH && maxH >= 4 {
		logoH = maxH
	}
	if logoW < 8 {
		logoW = 8
	}
	if logoH < 4 {
		logoH = 4
	}

	logoCols := (logoW + cellW - 1) / cellW
	logoRows := (logoH + cellH - 1) / cellH
	placeCols := min(termCols, logoCols+1)
	placeRows := min(termRows, logoRows+1)
	cropW := max(logoW, placeCols*cellW)
	cropH := max(logoH, placeRows*cellH)
	marginX := cropW - logoW
	marginY := cropH - logoH
	texW := marginX + cropW
	texH := marginY + cropH

	k.buf.Reset()
	k.buf.WriteString("\x1b[?2026h")
	// Delete any previous images and free terminal GPU/RAM cache.
	k.buf.WriteString("\x1b_Ga=d,d=A,q=2\x1b\\")

	mask := RasterizeDVDMask(logoW, logoH, includeVideo)
	base := k.nextBaseID
	k.nextBaseID += uint32(len(palette) + 8)

	k.colorIDs = k.colorIDs[:0]
	for i, col := range palette {
		id := base + uint32(i)
		img := mask.TintPaddedRGBA(col, texW, texH, marginX, marginY)
		if err := encodeKittyPNG(&k.buf, id, img); err != nil {
			return err
		}
		k.colorIDs = append(k.colorIDs, id)
	}

	k.flashID = base + uint32(len(palette))
	if err := encodeKittyPNG(&k.buf, k.flashID, mask.TintPaddedRGBA(flash, texW, texH, marginX, marginY)); err != nil {
		return err
	}

	k.buf.WriteString("\x1b[?2026l")
	k.activeImgID = 0
	k.logoW = logoW
	k.logoH = logoH
	k.cellW = cellW
	k.cellH = cellH
	k.placeCols = placeCols
	k.placeRows = placeRows
	k.cropW = cropW
	k.cropH = cropH
	k.marginX = marginX
	k.marginY = marginY

	_, err := k.out.Write(k.buf.Bytes())
	return err
}

// DrawFrame positions the requested palette image at exact pixel coordinates (px, py)
// using a constant (c x r) cell box and sliding source-crop (x, y, w, h) so the logo
// never stretches or jitters across cell boundaries.
func (k *KittyRenderer) DrawFrame(px, py, cols, rows int, colorIdx int, useFlash bool, hud string) error {
	if len(k.colorIDs) == 0 || k.cellW < 1 || k.cellH < 1 {
		return nil
	}

	maxPx := max(0, cols*k.cellW-k.logoW)
	maxPy := max(0, rows*k.cellH-k.logoH)
	if px < 0 {
		px = 0
	} else if px > maxPx {
		px = maxPx
	}
	if py < 0 {
		py = 0
	} else if py > maxPy {
		py = maxPy
	}

	maxCol0 := max(0, cols-k.placeCols)
	maxRow0 := max(0, rows-k.placeRows)

	col0 := min(px/k.cellW, maxCol0)
	row0 := min(py/k.cellH, maxRow0)

	subX := px - col0*k.cellW
	subY := py - row0*k.cellH
	if subX < 0 {
		subX = 0
	} else if subX > k.marginX {
		subX = k.marginX
	}
	if subY < 0 {
		subY = 0
	} else if subY > k.marginY {
		subY = k.marginY
	}

	srcX := k.marginX - subX
	srcY := k.marginY - subY

	targetID := k.colorIDs[colorIdx%len(k.colorIDs)]
	if useFlash {
		targetID = k.flashID
	}

	k.buf.Reset()
	k.buf.WriteString("\x1b[?2026h")

	if k.activeImgID != 0 && k.activeImgID != targetID {
		// Delete only the visible placement (d=i lowercase keeps cached image texture in RAM).
		k.buf.WriteString("\x1b_Ga=d,d=i,i=")
		writeUint(&k.buf, uint64(k.activeImgID))
		k.buf.WriteString(",p=1,q=2\x1b\\")
	}

	// Anchor cell (1-indexed) + fixed c/r cell span + source rectangle crop (x,y,w,h).
	k.buf.WriteString("\x1b[")
	writeUint(&k.buf, uint64(row0+1))
	k.buf.WriteByte(';')
	writeUint(&k.buf, uint64(col0+1))
	k.buf.WriteByte('H')

	k.buf.WriteString("\x1b_Ga=p,i=")
	writeUint(&k.buf, uint64(targetID))
	k.buf.WriteString(",p=")
	writeUint(&k.buf, kittyPlacementID)
	k.buf.WriteString(",x=")
	writeUint(&k.buf, uint64(srcX))
	k.buf.WriteString(",y=")
	writeUint(&k.buf, uint64(srcY))
	k.buf.WriteString(",w=")
	writeUint(&k.buf, uint64(k.cropW))
	k.buf.WriteString(",h=")
	writeUint(&k.buf, uint64(k.cropH))
	k.buf.WriteString(",c=")
	writeUint(&k.buf, uint64(k.placeCols))
	k.buf.WriteString(",r=")
	writeUint(&k.buf, uint64(k.placeRows))
	k.buf.WriteString(",C=1,q=2\x1b\\")

	if hud != "" && rows >= 2 {
		k.buf.WriteString("\x1b[")
		writeUint(&k.buf, uint64(rows))
		k.buf.WriteString(";1H\x1b[2K\x1b[38;5;242m ")
		if len(hud) > cols-2 && cols > 4 {
			hud = hud[:cols-2]
		}
		k.buf.WriteString(hud)
		k.buf.WriteString("\x1b[0m")
	}

	k.buf.WriteString("\x1b[?2026l")
	k.activeImgID = targetID

	_, err := k.out.Write(k.buf.Bytes())
	return err
}

// ClearHUDLine erases the bottom status line when toggled off.
func (k *KittyRenderer) ClearHUDLine(rows int) {
	if rows >= 1 {
		fmt.Fprintf(k.out, "\x1b[%d;1H\x1b[2K", rows)
	}
}

// ClearAll removes all placements and frees cached image data from terminal memory.
func (k *KittyRenderer) ClearAll() {
	_, _ = io.WriteString(k.out, "\x1b_Ga=d,d=A,q=2\x1b\\")
	k.activeImgID = 0
}

// encodeKittyPNG encodes img as PNG and appends 4096-byte base64-chunked Kitty transmission commands (a=t,f=100).
func encodeKittyPNG(dst *bytes.Buffer, id uint32, img image.Image) error {
	var pngBuf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&pngBuf, img); err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString(pngBuf.Bytes())

	const chunkSize = 4096
	for offset := 0; offset < len(b64); offset += chunkSize {
		end := offset + chunkSize
		more := 1
		if end >= len(b64) {
			end = len(b64)
			more = 0
		}
		dst.WriteString("\x1b_G")
		if offset == 0 {
			dst.WriteString("a=t,f=100,i=")
			writeUint(dst, uint64(id))
			dst.WriteString(",q=2,m=")
			writeUint(dst, uint64(more))
		} else {
			dst.WriteString("m=")
			writeUint(dst, uint64(more))
		}
		dst.WriteByte(';')
		dst.WriteString(b64[offset:end])
		dst.WriteString("\x1b\\")
	}
	return nil
}

func writeUint(dst *bytes.Buffer, v uint64) {
	var tmp [20]byte
	dst.Write(strconv.AppendUint(tmp[:0], v, 10))
}
