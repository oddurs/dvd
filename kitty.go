package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
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

// templateIndexedPalette initializes a 65-entry non-premultiplied palette (0..64 subpixel coverage)
// so a single 1-byte/pixel PNG encode can be re-tinted for every palette color in nanoseconds by
// patching only the 195-byte PLTE chunk + IEEE CRC32.
var templateIndexedPalette = func() color.Palette {
	pal := make(color.Palette, rasterMaxCoverage+1)
	for k := 0; k <= rasterMaxCoverage; k++ {
		pal[k] = color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: covToAlpha[k]}
	}
	return pal
}()

// KittyRenderer manages GPU-cached supersampled logo textures and sub-pixel smooth placements
// using the Kitty Graphics Protocol (directly or wrapped in tmux DCS passthrough).
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
	outBuf          []byte
	pngBuf          bytes.Buffer
	idxPix          []uint8
	b64Buf          []byte
	palImg          image.Paletted
	pngPool         pngBufPool
}

func NewKittyRenderer(out io.Writer, blackBG, tmuxPassthrough bool) *KittyRenderer {
	return &KittyRenderer{
		out:             out,
		blackBG:         blackBG,
		tmuxPassthrough: tmuxPassthrough,
		nextBaseID:      100,
		outBuf:          make([]byte, 0, 256),
	}
}

func (k *KittyRenderer) appendAPCStart(b []byte) []byte {
	if k.tmuxPassthrough {
		return append(b, "\x1bPtmux;\x1b\x1b_G"...)
	}
	return append(b, "\x1b_G"...)
}

func (k *KittyRenderer) appendAPCEnd(b []byte) []byte {
	if k.tmuxPassthrough {
		return append(b, "\x1b\x1b\\\x1b\\"...)
	}
	return append(b, "\x1b\\"...)
}

func (k *KittyRenderer) resetSGR() string {
	if k.blackBG {
		return "\x1b[0m\x1b[48;2;0;0;0m"
	}
	return "\x1b[0m"
}

// findPLTEChunk locates the PLTE chunk's 4-byte type header and RGB payload slice inside a valid PNG stream.
func findPLTEChunk(pngBytes []byte) (typeStart, dataStart, dataEnd int, ok bool) {
	if len(pngBytes) < 33 {
		return 0, 0, 0, false
	}
	pos := 8 // skip 8-byte PNG magic header
	for pos+12 <= len(pngBytes) {
		length := int(binary.BigEndian.Uint32(pngBytes[pos : pos+4]))
		if length < 0 || pos+12+length > len(pngBytes) {
			return 0, 0, 0, false
		}
		if pngBytes[pos+4] == 'P' && pngBytes[pos+5] == 'L' && pngBytes[pos+6] == 'T' && pngBytes[pos+7] == 'E' {
			if length > 0 && length%3 == 0 {
				return pos + 4, pos + 8, pos + 8 + length, true
			}
			return 0, 0, 0, false
		}
		pos += 12 + length
	}
	return 0, 0, 0, false
}

// patchPLTEColor updates all RGB entries in the PLTE chunk and recomputes its 4-byte IEEE CRC32.
func patchPLTEColor(pngBytes []byte, typeStart, dataStart, dataEnd int, col color.RGBA) {
	plte := pngBytes[dataStart:dataEnd]
	for i := 0; i+2 < len(plte); i += 3 {
		plte[i+0] = col.R
		plte[i+1] = col.G
		plte[i+2] = col.B
	}
	crc := crc32.ChecksumIEEE(pngBytes[typeStart:dataEnd])
	binary.BigEndian.PutUint32(pngBytes[dataEnd:dataEnd+4], crc)
}

// UploadPalette rasterizes the vector DVD logo at 2x supersampled texel density, encodes a single
// 1-byte/pixel indexed PNG, patches PLTE+CRC32 for each palette color in-place, and uploads all
// textures to terminal GPU memory.
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

	needPix := texW * texH
	if cap(k.idxPix) < needPix {
		k.idxPix = make([]uint8, needPix)
	} else {
		k.idxPix = k.idxPix[:needPix]
		clear(k.idxPix)
	}
	rasterizeDVDInto(k.idxPix, texW, marginX, marginY, sLogoW, sLogoH, includeVideo, &identityCovLUT)

	k.palImg = image.Paletted{
		Pix:     k.idxPix,
		Stride:  texW,
		Rect:    image.Rect(0, 0, texW, texH),
		Palette: templateIndexedPalette,
	}

	k.pngBuf.Reset()
	enc := png.Encoder{
		CompressionLevel: png.BestSpeed,
		BufferPool:       &k.pngPool,
	}
	if err := enc.Encode(&k.pngBuf, &k.palImg); err != nil {
		return err
	}
	pngBytes := k.pngBuf.Bytes()
	typeStart, dataStart, dataEnd, hasPLTE := findPLTEChunk(pngBytes)

	encLen := base64.StdEncoding.EncodedLen(len(pngBytes))
	if cap(k.b64Buf) < encLen {
		k.b64Buf = make([]byte, encLen)
	} else {
		k.b64Buf = k.b64Buf[:encLen]
	}

	base := k.nextBaseID
	k.nextBaseID += uint32(len(palette) + 8)

	b := append(k.outBuf[:0], "\x1b[?2026h"...)
	b = k.appendAPCStart(b)
	b = append(b, "a=d,d=A,q=2"...)
	b = k.appendAPCEnd(b)

	var fallbackMask *LogoMask
	emitColor := func(id uint32, col color.RGBA) error {
		if hasPLTE && col.A == 0xFF {
			patchPLTEColor(pngBytes, typeStart, dataStart, dataEnd, col)
			base64.StdEncoding.Encode(k.b64Buf, pngBytes)
			b = k.appendBase64Chunks(b, id, k.b64Buf)
			return nil
		}
		if fallbackMask == nil {
			fallbackMask = RasterizeDVDMask(sLogoW, sLogoH, includeVideo)
		}
		var fbBuf bytes.Buffer
		if err := enc.Encode(&fbBuf, fallbackMask.TintPaddedRGBA(col, texW, texH, marginX, marginY)); err != nil {
			return err
		}
		b64 := make([]byte, base64.StdEncoding.EncodedLen(fbBuf.Len()))
		base64.StdEncoding.Encode(b64, fbBuf.Bytes())
		b = k.appendBase64Chunks(b, id, b64)
		return nil
	}

	k.colorIDs = k.colorIDs[:0]
	for i, col := range palette {
		id := base + uint32(i)
		if err := emitColor(id, col); err != nil {
			return err
		}
		k.colorIDs = append(k.colorIDs, id)
	}

	k.flashID = base + uint32(len(palette))
	if err := emitColor(k.flashID, flash); err != nil {
		return err
	}

	b = append(b, "\x1b[?2026l"...)
	k.outBuf = b
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

	_, err := k.out.Write(b)
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

	b := append(k.outBuf[:0], "\x1b[?2026h"...)

	if k.activeImgID != 0 && k.activeImgID != targetID {
		// Delete only the visible placement (d=i lowercase keeps cached image texture in RAM).
		if k.tmuxPassthrough {
			b = append(b, "\x1bPtmux;\x1b\x1b_Ga=d,d=i,i="...)
		} else {
			b = append(b, "\x1b_Ga=d,d=i,i="...)
		}
		b = strconv.AppendUint(b, uint64(k.activeImgID), 10)
		if k.tmuxPassthrough {
			b = append(b, ",p=1,q=2\x1b\x1b\\\x1b\\"...)
		} else {
			b = append(b, ",p=1,q=2\x1b\\"...)
		}
	}

	// Anchor cell (1-indexed) + fixed c/r cell span + source rectangle crop (x,y,w,h) behind text (z=-1).
	b = append(b, "\x1b["...)
	b = strconv.AppendUint(b, uint64(row0+1), 10)
	b = append(b, ';')
	b = strconv.AppendUint(b, uint64(col0+1), 10)
	b = append(b, 'H')

	if k.tmuxPassthrough {
		b = append(b, "\x1bPtmux;\x1b\x1b_Ga=p,i="...)
	} else {
		b = append(b, "\x1b_Ga=p,i="...)
	}
	b = strconv.AppendUint(b, uint64(targetID), 10)
	b = append(b, ",p=1,x="...)
	b = strconv.AppendUint(b, uint64(srcX), 10)
	b = append(b, ",y="...)
	b = strconv.AppendUint(b, uint64(srcY), 10)
	b = append(b, ",w="...)
	b = strconv.AppendUint(b, uint64(w), 10)
	b = append(b, ",h="...)
	b = strconv.AppendUint(b, uint64(h), 10)
	b = append(b, ",c="...)
	b = strconv.AppendUint(b, uint64(c), 10)
	b = append(b, ",r="...)
	b = strconv.AppendUint(b, uint64(r), 10)
	if k.tmuxPassthrough {
		b = append(b, ",z=-1,C=1,q=2\x1b\x1b\\\x1b\\"...)
	} else {
		b = append(b, ",z=-1,C=1,q=2\x1b\\"...)
	}

	if hud != "" {
		rst := k.resetSGR()
		b = append(b, "\x1b["...)
		b = strconv.AppendUint(b, uint64(rows), 10)
		b = append(b, ";1H"...)
		b = append(b, rst...)
		b = append(b, "\x1b[2K\x1b[38;5;242m "...)
		b = append(b, hud...)
		b = append(b, rst...)
	}

	b = append(b, "\x1b[?2026l"...)
	k.outBuf = b
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

	_, err := k.out.Write(b)
	return err
}

// ClearHUDLine erases the bottom status line when toggled off.
func (k *KittyRenderer) ClearHUDLine(rows int) {
	k.prevHUD = ""
	if rows >= 1 {
		b := append(k.outBuf[:0], "\x1b[?2026h\x1b["...)
		b = strconv.AppendUint(b, uint64(rows), 10)
		b = append(b, ";1H"...)
		b = append(b, k.resetSGR()...)
		b = append(b, "\x1b[2K\x1b[?2026l"...)
		k.outBuf = b
		_, _ = k.out.Write(b)
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

func (k *KittyRenderer) appendBase64Chunks(b []byte, id uint32, b64 []byte) []byte {
	chunkSize := 4096
	if k.tmuxPassthrough {
		// Keep DCS passthrough packets compact (~1KB) for all tmux versions.
		chunkSize = 1024
	}
	for offset := 0; offset < len(b64); offset += chunkSize {
		end := offset + chunkSize
		more := byte('1')
		if end >= len(b64) {
			end = len(b64)
			more = '0'
		}
		b = k.appendAPCStart(b)
		if offset == 0 {
			b = append(b, "a=t,f=100,i="...)
			b = strconv.AppendUint(b, uint64(id), 10)
			b = append(b, ",q=2,m="...)
			b = append(b, more)
		} else {
			b = append(b, 'm', '=', more)
		}
		b = append(b, ';')
		b = append(b, b64[offset:end]...)
		b = k.appendAPCEnd(b)
	}
	return b
}
