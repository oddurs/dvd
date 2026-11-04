package main

import (
	"bytes"
	"encoding/base64"
	"image/color"
	"image/png"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRasterizeDVDMaskGeometry(t *testing.T) {
	const W, H = 320, 180
	mask := RasterizeDVDMask(W, H, true)
	if mask.Width != W || mask.Height != H {
		t.Fatalf("unexpected mask dimensions: %dx%d", mask.Width, mask.Height)
	}

	// Disc body left of hole should be solid opaque (both Sample and Sample4x4).
	discSolidX := int(math.Round(float64(W) * (35.0 / logoViewWidth)))
	discY := int(math.Round(float64(H) * (61.5 / logoViewHeightVideo)))
	if got := mask.Sample(discSolidX, discY); got < 240 {
		t.Fatalf("expected solid disc pixel at (%d,%d), got alpha=%d", discSolidX, discY, got)
	}
	if got4 := mask.Sample4x4(discSolidX, discY); got4 < 240 {
		t.Fatalf("expected solid 4x4 disc block at (%d,%d), got alpha=%d", discSolidX, discY, got4)
	}

	// Center hole inside elliptical disc should be cut out (alpha == 0) via even-odd rule.
	holeX := int(math.Round(float64(W) * (72.8 / logoViewWidth)))
	if got := mask.Sample(holeX, discY); got != 0 {
		t.Fatalf("expected transparent disc center hole at (%d,%d), got alpha=%d", holeX, discY, got)
	}

	// VIDEO sub-label region: verify opaque strokes, cutout counter inside 'O', and optical centering under disc.
	oCenterX := int(math.Round(float64(W) * (120.0 / logoViewWidth)))
	oCenterY := int(math.Round(float64(H) * (83.5 / logoViewHeightVideo)))
	if got := mask.Sample(oCenterX, oCenterY); got != 0 {
		t.Fatalf("expected hollow counter inside 'O' at (%d,%d), got alpha=%d", oCenterX, oCenterY, got)
	}

	minVideoX, maxVideoX, videoPixels := W, 0, 0
	for y := int(math.Round(float64(H) * 0.83)); y < H; y++ {
		for x := 0; x < W; x++ {
			if mask.Sample(x, y) > 128 {
				videoPixels++
				if x < minVideoX {
					minVideoX = x
				}
				if x > maxVideoX {
					maxVideoX = x
				}
			}
		}
	}
	if videoPixels < 50 {
		t.Fatalf("expected VIDEO sub-label pixels when includeVideo=true, got %d", videoPixels)
	}
	discMidPx := float64(W) * (75.6 / logoViewWidth)
	videoMidPx := float64(minVideoX+maxVideoX) / 2.0
	if math.Abs(videoMidPx-discMidPx) > 3.0 {
		t.Fatalf("VIDEO sub-label midpoint %.1f not centered under disc midpoint %.1f", videoMidPx, discMidPx)
	}

	// With includeVideo=false, top letters and disc still render cleanly.
	maskNoVideo := RasterizeDVDMask(320, 146, false)
	discYNoVid := int(math.Round(146.0 * (61.5 / logoViewHeightNoText)))
	if got := maskNoVideo.Sample(holeX, discYNoVid); got != 0 {
		t.Fatalf("expected transparent disc hole in no-video mask, got alpha=%d", got)
	}

	// Tiny/degenerate dimensions clamp cleanly without panicking.
	tiny := RasterizeDVDMask(1, 1, true)
	if tiny.Width != 4 || tiny.Height != 2 {
		t.Fatalf("expected minimum clamped mask 4x2, got %dx%d", tiny.Width, tiny.Height)
	}
	if tiny.Sample(-1, 0) != 0 || tiny.Sample(99, 99) != 0 {
		t.Fatalf("out-of-bounds Sample must return 0")
	}
	if tiny.Sample4x4(-8, 0) != 0 || tiny.Sample4x4(99, 99) != 0 {
		t.Fatalf("out-of-bounds Sample4x4 must return 0")
	}
}

func TestTintPaddedRGBAGeometry(t *testing.T) {
	mask := RasterizeDVDMask(80, 45, true)
	col := color.RGBA{R: 0xFF, G: 0x2A, B: 0x85, A: 0xFF}
	img := mask.TintPaddedRGBA(col, 120, 75, 20, 15)
	if img.Bounds().Dx() != 120 || img.Bounds().Dy() != 75 {
		t.Fatalf("unexpected padded bounds: %v", img.Bounds())
	}
	// Margin corners must remain fully transparent (A == 0) while carrying fringe-free RGB.
	wantTransparent := color.NRGBA{R: col.R, G: col.G, B: col.B, A: 0}
	for _, pt := range [][2]int{{0, 0}, {19, 14}, {119, 74}, {10, 70}} {
		if got := img.NRGBAAt(pt[0], pt[1]); got != wantTransparent {
			t.Fatalf("expected bleed-free transparent margin at %v, got %+v", pt, got)
		}
	}
	// Undersized canvas auto-expands so logo never clips.
	expanded := mask.TintPaddedRGBA(col, 10, 10, 5, 6)
	if expanded.Bounds().Dx() != 85 || expanded.Bounds().Dy() != 51 {
		t.Fatalf("expected auto-expanded bounds 85x51, got %v", expanded.Bounds())
	}
}

func TestKittyRendererChunkingPNGValidityAndSubpixelPlacements(t *testing.T) {
	var out bytes.Buffer
	kr := NewKittyRenderer(&out, true, false)

	pal := []color.RGBA{
		{R: 0, G: 153, B: 255, A: 255},
		{R: 255, G: 42, B: 133, A: 255},
	}
	if err := kr.UploadPalette(240, 135, 10, 20, 100, 40, true, pal, cornerFlashColor); err != nil {
		t.Fatalf("UploadPalette failed: %v", err)
	}

	uploadStream := out.String()
	if !strings.Contains(uploadStream, "\x1b_Ga=d,d=A,q=2\x1b\\") {
		t.Fatalf("expected UploadPalette to clear previous textures with d=A")
	}

	// Verify every APC payload chunk is <= 4096 bytes AND reassemble id=100 to verify 2x supersampled PNG dimensions.
	parts := strings.Split(uploadStream, "\x1b_G")
	var img100B64 strings.Builder
	collecting100 := false
	for _, part := range parts[1:] {
		end := strings.Index(part, "\x1b\\")
		if end < 0 {
			t.Fatalf("unterminated APC sequence in upload stream")
		}
		apc := part[:end]
		semi := strings.IndexByte(apc, ';')
		if semi < 0 {
			continue
		}
		ctrl, payload := apc[:semi], apc[semi+1:]
		if len(payload) > 4096 {
			t.Fatalf("APC base64 chunk exceeded 4096 bytes: %d", len(payload))
		}
		if strings.Contains(ctrl, "i=100,") {
			collecting100 = true
		}
		if collecting100 {
			img100B64.WriteString(payload)
			if strings.Contains(ctrl, "m=0") {
				collecting100 = false
			}
		}
	}

	pngBytes, err := base64.StdEncoding.DecodeString(img100B64.String())
	if err != nil {
		t.Fatalf("failed to base64-decode transmitted image 100: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("transmitted image 100 is not a valid PNG: %v", err)
	}
	// At 2x subpixel scale: texW = marginX(20) + cropW(500) = 520; texH = marginY(50) + cropH(320) = 370.
	if decoded.Bounds().Dx() != 520 || decoded.Bounds().Dy() != 370 {
		t.Fatalf("expected 2x supersampled padded PNG dimensions 520x370, got %v", decoded.Bounds())
	}

	// Frame 1: place color 0 (id=100) at pixel (27.0, 45.0).
	out.Reset()
	if err := kr.DrawFrame(27.0, 45.0, 100, 40, 0, false, "bounces: 1  •  corners: 0"); err != nil {
		t.Fatalf("DrawFrame failed: %v", err)
	}
	frame1 := out.String()
	if strings.Contains(frame1, "a=d") {
		t.Fatalf("first frame should not delete an existing placement: %q", frame1)
	}
	if strings.Contains(frame1, "X=") || strings.Contains(frame1, "Y=") {
		t.Fatalf("expected fixed-box crop placement without X=/Y= cell offsets: %q", frame1)
	}
	wantPut1 := "\x1b[3;3H\x1b_Ga=p,i=100,p=1,x=6,y=40,w=500,h=320,c=25,r=8,z=-1,C=1,q=2\x1b\\"
	if !strings.Contains(frame1, wantPut1) {
		t.Fatalf("unexpected frame1 placement:\ngot:  %q\nwant: %q", frame1, wantPut1)
	}
	if !strings.Contains(frame1, "\x1b[48;2;0;0;0m\x1b[2K") {
		t.Fatalf("expected blackBG HUD clear to preserve pitch-black SGR: %q", frame1)
	}

	// Duplicate unchanged frame emits 0 bytes.
	out.Reset()
	_ = kr.DrawFrame(27.0, 45.0, 100, 40, 0, false, "bounces: 1  •  corners: 0")
	if out.Len() != 0 {
		t.Fatalf("expected unchanged Kitty frame to write 0 bytes, got %d", out.Len())
	}

	// Half-pixel sub-step (27.5, 45.5) shifts UV crop by 1 supersampled texel (x=5, y=39).
	out.Reset()
	_ = kr.DrawFrame(27.5, 45.5, 100, 40, 0, false, "")
	wantHalf := "\x1b[3;3H\x1b_Ga=p,i=100,p=1,x=5,y=39,w=500,h=320,c=25,r=8,z=-1,C=1,q=2\x1b\\"
	if !strings.Contains(out.String(), wantHalf) {
		t.Fatalf("expected 0.5px sub-step placement:\ngot:  %q\nwant: %q", out.String(), wantHalf)
	}

	// Frame 2: same color moved across cell boundary to (30.0, 46.0).
	out.Reset()
	_ = kr.DrawFrame(30.0, 46.0, 100, 40, 0, false, "")
	frame2 := out.String()
	wantPut2 := "\x1b[3;4H\x1b_Ga=p,i=100,p=1,x=20,y=38,w=500,h=320,c=25,r=8,z=-1,C=1,q=2\x1b\\"
	if !strings.Contains(frame2, wantPut2) {
		t.Fatalf("unexpected frame2 placement:\ngot:  %q\nwant: %q", frame2, wantPut2)
	}

	// Frame 3: bounce changes color to colorIdx=1 (id=101) at exact bottom-right wall (px=760, py=665).
	out.Reset()
	_ = kr.DrawFrame(760.0, 665.0, 100, 40, 1, false, "")
	frame3 := out.String()
	wantDel := "\x1b_Ga=d,d=i,i=100,p=1,q=2\x1b\\"
	wantPut3 := "\x1b[33;76H\x1b_Ga=p,i=101,p=1,x=0,y=0,w=500,h=320,c=25,r=8,z=-1,C=1,q=2\x1b\\"
	if !strings.HasPrefix(frame3, "\x1b[?2026h") || !strings.HasSuffix(frame3, "\x1b[?2026l") {
		t.Fatalf("expected synchronized output wrapper on frame3: %q", frame3)
	}
	if !strings.Contains(frame3, wantDel) || !strings.Contains(frame3, wantPut3) {
		t.Fatalf("expected atomic delete+put on color change at bottom-right wall:\ngot:  %q\nwant: %q", frame3, wantPut3)
	}

	// Mid-resize safety: if cols temporarily shrinks below placeCols (cols=12 < 25), c clamps to 12.
	out.Reset()
	_ = kr.DrawFrame(50.0, 50.0, 12, 5, 1, true, "")
	if !strings.Contains(out.String(), ",c=12,r=5,") {
		t.Fatalf("expected mid-resize placement to clamp to 12x5, got %q", out.String())
	}
}

func TestKittyRendererTmuxPassthrough(t *testing.T) {
	var out bytes.Buffer
	kr := NewKittyRenderer(&out, false, true)
	pal := []color.RGBA{{R: 0, G: 153, B: 255, A: 255}}

	if err := kr.UploadPalette(200, 112, 10, 20, 80, 30, true, pal, cornerFlashColor); err != nil {
		t.Fatalf("UploadPalette tmux failed: %v", err)
	}

	stream := out.String()
	if !strings.Contains(stream, "\x1bPtmux;\x1b\x1b_Ga=d,d=A,q=2\x1b\x1b\\\x1b\\") {
		t.Fatalf("expected tmux DCS-wrapped clear-all command in UploadPalette")
	}

	// Every DCS passthrough chunk must use <= 1024 base64 bytes and terminate cleanly with \x1b\x1b\\\x1b\\.
	dcsBlocks := strings.Split(stream, "\x1bPtmux;\x1b\x1b_G")
	for _, blk := range dcsBlocks[1:] {
		end := strings.Index(blk, "\x1b\x1b\\\x1b\\")
		if end < 0 {
			t.Fatalf("unterminated tmux DCS passthrough block")
		}
		apc := blk[:end]
		if semi := strings.IndexByte(apc, ';'); semi >= 0 {
			payload := apc[semi+1:]
			if len(payload) > 1024 {
				t.Fatalf("tmux DCS payload exceeded 1024 bytes: %d", len(payload))
			}
		}
	}

	// DrawFrame keeps pane-relative cursor CUP unwrapped while wrapping the Kitty placement in DCS.
	out.Reset()
	if err := kr.DrawFrame(25.0, 40.0, 80, 30, 0, false, ""); err != nil {
		t.Fatalf("DrawFrame tmux failed: %v", err)
	}
	frame := out.String()
	if !strings.Contains(frame, "\x1b[3;3H\x1bPtmux;\x1b\x1b_Ga=p,i=100,p=1,") {
		t.Fatalf("expected unwrapped pane CUP followed by DCS-wrapped Kitty placement, got %q", frame)
	}

	out.Reset()
	kr.ClearAll()
	if out.String() != "\x1bPtmux;\x1b\x1b_Ga=d,d=A,q=2\x1b\x1b\\\x1b\\" {
		t.Fatalf("unexpected tmux ClearAll sequence: %q", out.String())
	}
}

func TestKittyRendererFullViewportContinuitySweep(t *testing.T) {
	var out bytes.Buffer
	kr := NewKittyRenderer(&out, false, false)
	const cols, rows, cellW, cellH = 80, 28, 17, 38
	const logoW, logoH = 290, 164
	pal := []color.RGBA{{R: 0, G: 153, B: 255, A: 255}}

	if err := kr.UploadPalette(logoW, logoH, cellW, cellH, cols, rows, true, pal, cornerFlashColor); err != nil {
		t.Fatalf("UploadPalette: %v", err)
	}

	re := regexp.MustCompile(`\x1b\[(\d+);(\d+)H\x1b_Ga=p,i=\d+,p=1,x=(\d+),y=(\d+),w=(\d+),h=(\d+),c=(\d+),r=(\d+),z=-1,C=1,q=2\x1b\\`)
	maxTx := (cols*cellW - logoW) * kittySubpixelScale
	maxTy := (rows*cellH - logoH) * kittySubpixelScale
	sCellW := cellW * kittySubpixelScale
	sCellH := cellH * kittySubpixelScale

	// Sweep every single 0.5-pixel sub-step across the viewport and verify exact reconstruction and constant box.
	for tx := 0; tx <= maxTx; tx++ {
		ty := (tx * maxTy) / maxTx
		fx := float64(tx) / float64(kittySubpixelScale)
		fy := float64(ty) / float64(kittySubpixelScale)
		out.Reset()
		if err := kr.DrawFrame(fx, fy, cols, rows, 0, false, ""); err != nil {
			t.Fatalf("DrawFrame(%.1f,%.1f): %v", fx, fy, err)
		}
		if out.Len() == 0 {
			continue
		}
		m := re.FindStringSubmatch(out.String())
		if m == nil {
			t.Fatalf("unexpected escape sequence at (%.1f,%.1f): %q", fx, fy, out.String())
		}
		row1, _ := strconv.Atoi(m[1])
		col1, _ := strconv.Atoi(m[2])
		srcX, _ := strconv.Atoi(m[3])
		srcY, _ := strconv.Atoi(m[4])
		w, _ := strconv.Atoi(m[5])
		h, _ := strconv.Atoi(m[6])
		c, _ := strconv.Atoi(m[7])
		r, _ := strconv.Atoi(m[8])

		if c != kr.placeCols || r != kr.placeRows || w != kr.cropW || h != kr.cropH {
			t.Fatalf("box mutated at (%.1f,%.1f): got (%d,%d,%d,%d)", fx, fy, c, r, w, h)
		}
		if col1 < 1 || col1+c-1 > cols {
			t.Fatalf("column out of bounds at fx=%.1f: col1=%d c=%d cols=%d", fx, col1, c, cols)
		}
		if row1 < 1 || row1+r-1 > rows {
			t.Fatalf("row out of bounds at fy=%.1f: row1=%d r=%d rows=%d", fy, row1, r, rows)
		}
		gotTx := (col1-1)*sCellW + (kr.marginX - srcX)
		gotTy := (row1-1)*sCellH + (kr.marginY - srcY)
		if gotTx != tx || gotTy != ty {
			t.Fatalf("subpixel position mismatch: want (%d,%d), reconstructed (%d,%d)", tx, ty, gotTx, gotTy)
		}
	}
}

func TestANSIRendererSubCellDifferentialEraseBlackBGAndNoColor(t *testing.T) {
	var out bytes.Buffer
	ar := NewANSIRenderer(&out, false, true, false)
	ar.ResizeLogo(20, 6, true)

	col := dvdPalette[0]
	// Aligned cell position (4.0, 2.0) spans 20x6 cells.
	if err := ar.DrawFrame(4.0, 2.0, 60, 20, col, "bounces: 3  •  corners: 1"); err != nil {
		t.Fatalf("DrawFrame failed: %v", err)
	}
	f1 := out.String()
	if !strings.Contains(f1, "▀") && !strings.Contains(f1, "▄") {
		t.Fatalf("expected half-block glyphs in ANSI frame")
	}
	if !strings.Contains(f1, "\x1b[48;2;0;0;0m") {
		t.Fatalf("expected pitch-black background SGR when blackBG=true")
	}
	if ar.prevW != 20 || ar.prevH != 6 {
		t.Fatalf("expected aligned box 20x6, got %dx%d", ar.prevW, ar.prevH)
	}

	// Duplicate unchanged sub-cell emits 0 bytes.
	out.Reset()
	_ = ar.DrawFrame(4.0, 2.0, 60, 20, col, "bounces: 3  •  corners: 1")
	if out.Len() != 0 {
		t.Fatalf("expected unchanged ANSI frame to emit 0 bytes, got %d", out.Len())
	}

	// Fractional sub-cell (4.25, 2.125) spans 21x7 cells cleanly.
	out.Reset()
	_ = ar.DrawFrame(4.25, 2.125, 60, 20, col, "")
	if ar.prevW != 21 || ar.prevH != 7 {
		t.Fatalf("expected fractional sub-cell box 21x7, got %dx%d", ar.prevW, ar.prevH)
	}

	// Moving down-right to (6.0, 3.0) erases trailing top row (row 3) and trailing left strip (cols 5..6)
	// with blackBG preserved without blanking overlapping interior cells with full-width spaces.
	out.Reset()
	_ = ar.DrawFrame(6.0, 3.0, 60, 20, col, "")
	f2 := out.String()
	if !strings.Contains(f2, "\x1b[3;5H\x1b[0m\x1b[48;2;0;0;0m                     ") {
		t.Fatalf("expected differential erase of uncovered top row 3 (21 spaces) with blackBG")
	}
	if !strings.Contains(f2, "\x1b[4;5H\x1b[0m\x1b[48;2;0;0;0m  \x1b[") {
		t.Fatalf("expected differential erase of uncovered 2-col left strip on row 4")
	}

	// ASCII + NO_COLOR strips all RGB TrueColor sequences.
	out.Reset()
	asciiNoCol := NewANSIRenderer(&out, true, true, true)
	asciiNoCol.ResizeLogo(22, 9, true)
	_ = asciiNoCol.DrawFrame(2.0, 1.0, 50, 15, col, "bounces: 1")
	f3 := out.String()
	if strings.Contains(f3, "38;2;") || strings.Contains(f3, "48;2;") || strings.Contains(f3, "38;5;") {
		t.Fatalf("expected NO_COLOR ASCII mode to emit zero color SGR codes, got %q", f3)
	}
	if !strings.Contains(f3, "V  I  D  E  O") {
		t.Fatalf("expected pure ASCII art DVD VIDEO text, got %q", f3)
	}
}

func TestUTF8SafeHUDTruncation(t *testing.T) {
	hud := "bounces: 42  •  corners: 2  •  next corner: 12.4s (top-right)  •  mode: kitty-graphics (1.0x)"
	for width := 0; width <= len(hud)+5; width++ {
		got := truncateRunes(hud, width)
		if !utf8.ValidString(got) {
			t.Fatalf("truncateRunes produced invalid UTF-8 at width=%d: %q", width, got)
		}
		if n := utf8.RuneCountInString(got); n > width {
			t.Fatalf("truncateRunes exceeded width=%d: got %d runes", width, n)
		}
	}
}

func TestStatefulInputParserSplitPacketsMouseAndJobControl(t *testing.T) {
	ch := make(chan InputEvent, 32)
	var parser InputParser

	// Stream containing APC reply, DCS reply, OSC reply, CSI 16t, CSI 14t, DA1, focus in/out,
	// SGR mouse motion + click + release, SS3 Up arrow, multi-byte UTF-8 'é', Ctrl+L, Ctrl+Z,
	// 'r', 'c', and trailing lone Esc — fed ONE BYTE AT A TIME!
	stream := []byte(
		"\x1b_Gi=31;OK\x1b\\" +
			"\x1bP1$r0m\x1b\\" +
			"\x1b]11;rgb:1414/1616/1b1b\x07" +
			"\x1b[6;38;17t\x1b[4;1368;2040t\x1b[?62;22c\x1b[I\x1b[O" +
			"\x1b[<35;42;17M\x1b[<0;10;5M\x1b[<0;10;5m" +
			"\x1bOAé\x0c\x1arc\x1b",
	)
	for _, b := range stream {
		parser.Feed([]byte{b}, ch)
	}
	if !parser.NeedsTimeout() {
		t.Fatalf("expected trailing lone Esc byte to await disambiguation timeout")
	}
	parser.FlushTimeout(ch)

	want := []InputEvent{
		{CellW: 17, CellH: 38},
		{Action: ActionMouse, MouseBtn: 35, MouseCol: 42, MouseRow: 17, MouseMotion: true},
		{Action: ActionMouse, MouseBtn: 0, MouseCol: 10, MouseRow: 5},
		{Action: ActionMouse, MouseBtn: 0, MouseCol: 10, MouseRow: 5, MouseRelease: true},
		{Action: ActionSpeedUp},
		{Action: ActionAnyOtherKey}, // single event for 2-byte UTF-8 'é'
		{Action: ActionRedraw},      // Ctrl+L
		{Action: ActionSuspend},     // Ctrl+Z
		{Action: ActionRandomize},   // 'r'
		{Action: ActionAimCorner},   // 'c'
		{Action: ActionQuit},        // flushed lone Esc
	}
	if len(ch) != len(want) {
		t.Fatalf("event count mismatch: got %d, want %d", len(ch), len(want))
	}
	for i, w := range want {
		got := <-ch
		if got != w {
			t.Fatalf("event[%d]: got %+v, want %+v", i, got, w)
		}
	}
}

func TestCSIParsingAndDA1Validation(t *testing.T) {
	// Malformed earlier prefix should not shadow valid CSI 16t response.
	if cw, ch := parseCSI16t("\x1b[6;badt\x1b[6;36;16t"); cw != 16 || ch != 36 {
		t.Fatalf("expected 16x36 after skipping malformed prefix, got %dx%d", cw, ch)
	}
	if ww, wh := parseCSI14t("\x1b[4;1080;1920t"); ww != 1920 || wh != 1080 {
		t.Fatalf("expected 1920x1080 from CSI 14t, got %dx%d", ww, wh)
	}
	if !hasDA1Response([]byte("\x1b_Gi=31;OK\x1b\\\x1b[?62;22c")) {
		t.Fatalf("expected valid DA1 detection")
	}
	if hasDA1Response([]byte("\x1b[?2026h_not_da1_c")) {
		t.Fatalf("expected non-digit sequence with 'c' not to falsely match DA1")
	}
	if wrapped := wrapTmuxDCS("\x1b_Ga=d\x1b\\"); wrapped != "\x1bPtmux;\x1b\x1b_Ga=d\x1b\x1b\\\x1b\\" {
		t.Fatalf("unexpected wrapTmuxDCS output: %q", wrapped)
	}
}

func TestPhysicsAimPredictAndCornerCelebration(t *testing.T) {
	for trial := 0; trial < 30; trial++ {
		p := NewPhysics(1200, 720, 320, 8)
		p.X = 150 + float64(trial*37%800)
		p.Y = 110 + float64(trial*53%450)
		if trial%2 == 0 {
			p.VX = -math.Abs(p.VX)
		}
		if trial%3 == 0 {
			p.VY = -math.Abs(p.VY)
		}
		origSpeed := math.Hypot(p.VX, p.VY)

		p.AimForCorner()
		predSec, predCorner, ok := p.PredictNextCorner(50)
		if !ok {
			t.Fatalf("trial %d: expected PredictNextCorner to find upcoming corner after AimForCorner", trial)
		}

		const dt = 1.0 / 240.0
		maxSteps := int(math.Ceil((predSec+1.0)/dt)) + 10
		hitAt := -1.0
		var hitCorner string
		prevColor := p.ColorIdx

		for step := 1; step <= maxSteps; step++ {
			ev := p.Step(dt)
			if math.Abs(math.Hypot(p.VX, p.VY)-origSpeed) > 1e-6 {
				t.Fatalf("speed drifted after step %d", step)
			}
			if ev.BouncedWall {
				if p.ColorIdx == prevColor {
					t.Fatalf("expected color to change on bounce, stayed at %d", p.ColorIdx)
				}
				prevColor = p.ColorIdx
			}
			if ev.HitCorner {
				hitAt = float64(step) * dt
				hitCorner = ev.CornerName
				_, flash := p.ActiveDisplayColor()
				if !flash {
					t.Fatalf("expected white flash on initial corner-hit frame")
				}
				break
			}
		}

		if hitAt < 0 {
			t.Fatalf("trial %d: AimForCorner predicted %s at %.3fs, but simulation did not hit corner", trial, predCorner, predSec)
		}
		if math.Abs(hitAt-predSec) > 0.05 {
			t.Fatalf("trial %d: predicted corner at %.3fs, actual %.3fs", trial, predSec, hitAt)
		}
		if hitCorner != predCorner {
			t.Fatalf("trial %d: predicted corner %q, actual %q", trial, predCorner, hitCorner)
		}
	}

	// Verify ETA minute rollover formatting (e.g. 119.6s -> 2m00s, not 1m00s).
	pSlow := NewPhysics(1000, 1000, 10, 8)
	pSlow.X, pSlow.Y = 0, 0
	pSlow.VX, pSlow.VY = 1000.0/119.6, 1000.0/119.6
	if got := formatNextCornerETA(pSlow); !strings.Contains(got, "2m00s") {
		t.Fatalf("expected 119.6s ETA to round to 2m00s, got %q", got)
	}
}

func TestZeroAllocRenderHotPaths(t *testing.T) {
	for _, tmux := range []bool{false, true} {
		kr := NewKittyRenderer(io.Discard, true, tmux)
		_ = kr.UploadPalette(320, 180, 16, 36, 120, 36, true, dvdPalette, cornerFlashColor)
		hud := "bounces: 12  •  corners: 1  •  next corner: 9.4s (bottom-right)"

		step := 0
		kittyAllocs := testing.AllocsPerRun(200, func() {
			step++
			fx := float64(step%400) * 0.5
			fy := float64(step%300) * 0.5
			_ = kr.DrawFrame(fx, fy, 120, 36, step%8, false, hud)
		})
		if kittyAllocs != 0 {
			t.Fatalf("expected 0 allocs/frame in KittyRenderer.DrawFrame (tmux=%v), got %.1f", tmux, kittyAllocs)
		}
	}

	ar := NewANSIRenderer(io.Discard, false, true, false)
	ar.ResizeLogo(24, 6, true)
	hud := "bounces: 12  •  corners: 1  •  next corner: 9.4s (bottom-right)"
	_ = ar.DrawFrame(5.25, 3.125, 120, 36, dvdPalette[0], hud) // warm up internal buffer

	step := 0
	ansiAllocs := testing.AllocsPerRun(200, func() {
		step++
		colF := float64(step%160) * 0.25
		rowF := float64(step%120) * 0.125
		_ = ar.DrawFrame(colF, rowF, 120, 36, dvdPalette[step%8], hud)
	})
	if ansiAllocs != 0 {
		t.Fatalf("expected 0 allocs/frame in ANSIRenderer.DrawFrame, got %.1f", ansiAllocs)
	}
}

func FuzzInputParser(f *testing.F) {
	f.Add([]byte("\x1b_Gi=31;OK\x1b\\\x1b[6;32;16t\x1b[?62;22cq"))
	f.Add([]byte("\x1b[A\x1b[B\x1bOA\x1b\x1b\x0c\x1a +-[ ]mnr?"))
	f.Add([]byte("\x1b[<35;120;40M\x1b[<0;1;1m\x1bP1$r0m\x1b\\\x1b]11;rgb:00/00/00\x07"))
	f.Add([]byte{0x1b, '[', '9', '9', '9', '9', '9', '9', ';', '1', 't'})
	f.Add([]byte{0xc3, 0xa9, 0xf0, 0x9f, 0x92, 0xbf, 0xff, 0x1b, '_'})

	f.Fuzz(func(t *testing.T, data []byte) {
		ch := make(chan InputEvent, len(data)+4)
		var p InputParser
		p.Feed(data, ch)
		p.FlushTimeout(ch)
	})
}

func FuzzCSITwoParams(f *testing.F) {
	f.Add("\x1b[6;36;16t")
	f.Add("\x1b[4;1080;1920t")
	f.Add("\x1b[<35;80;24M")
	f.Add("\x1b[6;;t\x1b[6;999999999;1t")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parseCSI16t(s)
		_, _ = parseCSI14t(s)
		_, _ = parseSGRMouse(s)
		_ = hasDA1Response([]byte(s))
	})
}

func BenchmarkRasterizeDVDMask(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = RasterizeDVDMask(420, 237, true)
	}
}

func BenchmarkKittyUploadPalette(b *testing.B) {
	kr := NewKittyRenderer(io.Discard, false, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = kr.UploadPalette(420, 237, 16, 36, 120, 36, true, dvdPalette, cornerFlashColor)
	}
}

func BenchmarkKittyDrawFrame(b *testing.B) {
	kr := NewKittyRenderer(io.Discard, false, false)
	_ = kr.UploadPalette(420, 237, 16, 36, 120, 36, true, dvdPalette, cornerFlashColor)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = kr.DrawFrame(float64(i%1600)*0.5, float64(i%1000)*0.5, 120, 36, i%8, false, "bounces: 5  •  corners: 0")
	}
}

func BenchmarkANSIDrawFrame(b *testing.B) {
	ar := NewANSIRenderer(io.Discard, false, false, false)
	ar.ResizeLogo(25, 6, true)
	_ = ar.DrawFrame(0.25, 0.125, 120, 36, dvdPalette[0], "")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ar.DrawFrame(float64(i%320)*0.25, float64(i%200)*0.125, 120, 36, dvdPalette[i%8], "bounces: 5  •  corners: 0")
	}
}
