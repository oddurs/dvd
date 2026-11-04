package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"strconv"
	"sync"
)

const (
	kittyPlacementID   = 1
	kittySubpixelScale = 2 // 2x2 texels per physical pixel -> 0.5px bilinear UV sub-stepping
)

type pngBufPool struct {
	mu   sync.Mutex
	bufs []*png.EncoderBuffer
}

func (p *pngBufPool) Get() *png.EncoderBuffer {
	p.mu.Lock()
	n := len(p.bufs)
	if n == 0 {
		p.mu.Unlock()
		return &png.EncoderBuffer{}
	}
	b := p.bufs[n-1]
	p.bufs = p.bufs[:n-1]
	p.mu.Unlock()
	return b
}

func (p *pngBufPool) Put(b *png.EncoderBuffer) {
	p.mu.Lock()
	p.bufs = append(p.bufs, b)
	p.mu.Unlock()
}

// KittyRenderer manages GPU-cached supersampled logo textures and sub-pixel smooth placements
// using the Kitty Graphics Protocol (directly or wrapped in tmux DCS passthrough).
//
// Each texture is rasterized at 2x sub-pixel density inside a padded transparent margin and
// displayed into a FIXED (c x r) cell box via source-rectangle cropping (x, y, w, h).
// That keeps the rendered cell box 100% constant (zero jelly stretching) while sliding smoothly
// in half-pixel GPU bilinear increments across cell boundaries.
type KittyRenderer struct {
	out             io.Writer
	blackBG         bool
	tmuxPassthrough bool
	nextBaseID      uint32
	colorIDs        []uint32
	flashID         uint32
	activeImgID     uint32
	logoW, logoH    int // viewport pixel dimensions
	cellW, cellH    int // viewport cell pixel dimensions
	placeCols       int
	placeRows       int
	cropW, cropH    int // in supersampled texels (kittySubpixelScale * px)
	marginX         int // in supersampled texels
	marginY         int // in supersampled texels
	hasPrev         bool
	prevCol0        int
	prevRow0        int
	prevSrcX        int
	prevSrcY        int
	prevW, prevH    int
	prevC, prevR    int
	prevHUD         string
	buf             bytes.Buffer
	pngPool         pngBufPool
}

func NewKittyRenderer(out io.Writer, blackBG, tmuxPassthrough bool) *KittyRenderer {
	return &KittyRenderer{
		out:             out,
		blackBG:         blackBG,
		tmuxPassthrough: tmuxPassthrough,
		nextBaseID:      100,
	}
}

func (k *KittyRenderer) writeAPCStart() {
	if k.tmuxPassthrough {
		k.buf.WriteString("\x1bPtmux;\x1b\x1b_G")
	} else {
		k.buf.WriteString("\x1b_G")
	}
}

func (k *KittyRenderer) writeAPCEnd() {
	if k.tmuxPassthrough {
		k.buf.WriteString("\x1b\x1b\\\x1b\\")
	} else {
		k.buf.WriteString("\x1b\\")
	}
}

func (k *KittyRenderer) resetSGR() string {
	if k.blackBG {
		return "\x1b[0m\x1b[48;2;0;0;0m"
	}
	return "\x1b[0m"
}

// UploadPalette rasterizes the vector DVD logo at 2x supersampled texel density inside a padded
// transparent atlas aligned to (cellW, cellH), encodes all palette PNGs in parallel, and uploads
// them to terminal GPU memory.
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

	sLogoW := logoW * kittySubpixelScale
	sLogoH := logoH * kittySubpixelScale
	sCellW := cellW * kittySubpixelScale
	sCellH := cellH * kittySubpixelScale

	logoCols := (sLogoW + sCellW - 1) / sCellW
	logoRows := (sLogoH + sCellH - 1) / sCellH
	placeCols := min(termCols, logoCols+1)
	placeRows := min(termRows, logoRows+1)
	cropW := max(sLogoW, placeCols*sCellW)
	cropH := max(sLogoH, placeRows*sCellH)
	marginX := cropW - sLogoW
	marginY := cropH - sLogoH
	texW := marginX + cropW
	texH := marginY + cropH

	mask := RasterizeDVDMask(sLogoW, sLogoH, includeVideo)
	base := k.nextBaseID
	k.nextBaseID += uint32(len(palette) + 8)

	total := len(palette) + 1
	b64Payloads := make([][]byte, total)
	errs := make([]error, total)

	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		col := flash
		if i < len(palette) {
			col = palette[i]
		}
		go func(idx int, c color.RGBA) {
			defer wg.Done()
			img := mask.TintPaddedRGBA(c, texW, texH, marginX, marginY)
			b64, err := k.encodePNGToBase64(img)
			b64Payloads[idx] = b64
			errs[idx] = err
		}(i, col)
	}
	wg.Wait()

	k.buf.Reset()
	k.buf.WriteString("\x1b[?2026h")
	// Delete any previous images and free terminal GPU/RAM cache.
	k.writeAPCStart()
	k.buf.WriteString("a=d,d=A,q=2")
	k.writeAPCEnd()

	k.colorIDs = k.colorIDs[:0]
	for i := 0; i < len(palette); i++ {
		if errs[i] != nil {
			return errs[i]
		}
		id := base + uint32(i)
		k.writeBase64Chunks(id, b64Payloads[i])
		k.colorIDs = append(k.colorIDs, id)
	}

	if errs[len(palette)] != nil {
		return errs[len(palette)]
	}
	k.flashID = base + uint32(len(palette))
	k.writeBase64Chunks(k.flashID, b64Payloads[len(palette)])

	k.buf.WriteString("\x1b[?2026l")
	k.activeImgID = 0
	k.hasPrev = false
	k.prevHUD = ""
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

// DrawFrame positions the requested palette image at floating-point pixel coordinates (fx, fy)
// quantized to half-pixel supersampled UV increments inside a constant (c x r) cell box.
func (k *KittyRenderer) DrawFrame(fx, fy float64, cols, rows int, colorIdx int, useFlash bool, hud string) error {
	if len(k.colorIDs) == 0 || k.cellW < 1 || k.cellH < 1 || cols < 1 || rows < 1 {
		return nil
	}

	sCellW := k.cellW * kittySubpixelScale
	sCellH := k.cellH * kittySubpixelScale
	sLogoW := k.logoW * kittySubpixelScale
	sLogoH := k.logoH * kittySubpixelScale

	c := min(k.placeCols, cols)
	r := min(k.placeRows, rows)
	w := min(k.cropW, c*sCellW)
	h := min(k.cropH, r*sCellH)

	maxTx := max(0, cols*sCellW-sLogoW)
	maxTy := max(0, rows*sCellH-sLogoH)
	tx := min(max(0, int(math.Round(fx*kittySubpixelScale))), maxTx)
	ty := min(max(0, int(math.Round(fy*kittySubpixelScale))), maxTy)

	maxCol0 := max(0, cols-c)
	maxRow0 := max(0, rows-r)

	col0 := min(tx/sCellW, maxCol0)
	row0 := min(ty/sCellH, maxRow0)

	subX := min(max(0, tx-col0*sCellW), k.marginX)
	subY := min(max(0, ty-row0*sCellH), k.marginY)

	srcX := k.marginX - subX
	srcY := k.marginY - subY

	targetID := k.colorIDs[((colorIdx%len(k.colorIDs))+len(k.colorIDs))%len(k.colorIDs)]
	if useFlash {
		targetID = k.flashID
	}

	if hud != "" && rows >= 2 && cols > 4 {
		hud = truncateRunes(hud, cols-2)
	} else {
		hud = ""
	}

	if k.hasPrev && k.activeImgID == targetID &&
		col0 == k.prevCol0 && row0 == k.prevRow0 &&
		srcX == k.prevSrcX && srcY == k.prevSrcY &&
		w == k.prevW && h == k.prevH &&
		c == k.prevC && r == k.prevR &&
		hud == k.prevHUD {
		return nil
	}

	k.buf.Reset()
	k.buf.WriteString("\x1b[?2026h")

	if k.activeImgID != 0 && k.activeImgID != targetID {
		// Delete only the visible placement (d=i lowercase keeps cached image texture in RAM).
		k.writeAPCStart()
		k.buf.WriteString("a=d,d=i,i=")
		writeUint(&k.buf, uint64(k.activeImgID))
		k.buf.WriteString(",p=1,q=2")
		k.writeAPCEnd()
	}

	// Anchor cell (1-indexed) + fixed c/r cell span + source rectangle crop (x,y,w,h) behind text (z=-1).
	k.buf.WriteString("\x1b[")
	writeUint(&k.buf, uint64(row0+1))
	k.buf.WriteByte(';')
	writeUint(&k.buf, uint64(col0+1))
	k.buf.WriteByte('H')

	k.writeAPCStart()
	k.buf.WriteString("a=p,i=")
	writeUint(&k.buf, uint64(targetID))
	k.buf.WriteString(",p=")
	writeUint(&k.buf, kittyPlacementID)
	k.buf.WriteString(",x=")
	writeUint(&k.buf, uint64(srcX))
	k.buf.WriteString(",y=")
	writeUint(&k.buf, uint64(srcY))
	k.buf.WriteString(",w=")
	writeUint(&k.buf, uint64(w))
	k.buf.WriteString(",h=")
	writeUint(&k.buf, uint64(h))
	k.buf.WriteString(",c=")
	writeUint(&k.buf, uint64(c))
	k.buf.WriteString(",r=")
	writeUint(&k.buf, uint64(r))
	k.buf.WriteString(",z=-1,C=1,q=2")
	k.writeAPCEnd()

	if hud != "" {
		k.buf.WriteString("\x1b[")
		writeUint(&k.buf, uint64(rows))
		k.buf.WriteString(";1H")
		k.buf.WriteString(k.resetSGR())
		k.buf.WriteString("\x1b[2K\x1b[38;5;242m ")
		k.buf.WriteString(hud)
		k.buf.WriteString(k.resetSGR())
	}

	k.buf.WriteString("\x1b[?2026l")
	k.activeImgID = targetID
	k.hasPrev = true
	k.prevCol0 = col0
	k.prevRow0 = row0
	k.prevSrcX = srcX
	k.prevSrcY = srcY
	k.prevW = w
	k.prevH = h
	k.prevC = c
	k.prevR = r
	k.prevHUD = hud

	_, err := k.out.Write(k.buf.Bytes())
	return err
}

// ClearHUDLine erases the bottom status line when toggled off.
func (k *KittyRenderer) ClearHUDLine(rows int) {
	k.prevHUD = ""
	if rows >= 1 {
		k.buf.Reset()
		k.buf.WriteString("\x1b[?2026h\x1b[")
		writeUint(&k.buf, uint64(rows))
		k.buf.WriteString(";1H")
		k.buf.WriteString(k.resetSGR())
		k.buf.WriteString("\x1b[2K\x1b[?2026l")
		_, _ = k.out.Write(k.buf.Bytes())
	}
}

// ClearAll removes all placements and frees cached image data from terminal memory.
func (k *KittyRenderer) ClearAll() {
	if k.tmuxPassthrough {
		_, _ = io.WriteString(k.out, "\x1bPtmux;\x1b\x1b_Ga=d,d=A,q=2\x1b\x1b\\\x1b\\")
	} else {
		_, _ = io.WriteString(k.out, "\x1b_Ga=d,d=A,q=2\x1b\\")
	}
	k.activeImgID = 0
	k.hasPrev = false
	k.prevHUD = ""
}

func (k *KittyRenderer) encodePNGToBase64(img image.Image) ([]byte, error) {
	var pngBuf bytes.Buffer
	enc := png.Encoder{
		CompressionLevel: png.BestSpeed,
		BufferPool:       &k.pngPool,
	}
	if err := enc.Encode(&pngBuf, img); err != nil {
		return nil, err
	}
	b64 := make([]byte, base64.StdEncoding.EncodedLen(pngBuf.Len()))
	base64.StdEncoding.Encode(b64, pngBuf.Bytes())
	return b64, nil
}

func (k *KittyRenderer) writeBase64Chunks(id uint32, b64 []byte) {
	chunkSize := 4096
	if k.tmuxPassthrough {
		// Keep DCS passthrough packets compact (~1KB) for all tmux versions.
		chunkSize = 1024
	}
	for offset := 0; offset < len(b64); offset += chunkSize {
		end := offset + chunkSize
		more := uint64(1)
		if end >= len(b64) {
			end = len(b64)
			more = 0
		}
		k.writeAPCStart()
		if offset == 0 {
			k.buf.WriteString("a=t,f=100,i=")
			writeUint(&k.buf, uint64(id))
			k.buf.WriteString(",q=2,m=")
			writeUint(&k.buf, more)
		} else {
			k.buf.WriteString("m=")
			writeUint(&k.buf, more)
		}
		k.buf.WriteByte(';')
		k.buf.Write(b64[offset:end])
		k.writeAPCEnd()
	}
}

func writeUint(dst *bytes.Buffer, v uint64) {
	var tmp [20]byte
	dst.Write(strconv.AppendUint(tmp[:0], v, 10))
}
