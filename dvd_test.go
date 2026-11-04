package main

import (
	"bytes"
	"image/color"
	"math"
	"strings"
	"testing"
)

func TestRasterizeDVDMaskGeometry(t *testing.T) {
	const W, H = 320, 180
	mask := RasterizeDVDMask(W, H, true)
	if mask.Width != W || mask.Height != H {
		t.Fatalf("unexpected mask dimensions: %dx%d", mask.Width, mask.Height)
	}

	// Disc body left of hole should be solid opaque.
	discSolidX := int(math.Round(float64(W) * (35.0 / logoViewWidth)))
	discY := int(math.Round(float64(H) * (61.5 / logoViewHeightVideo)))
	if got := mask.Sample(discSolidX, discY); got < 240 {
		t.Fatalf("expected solid disc pixel at (%d,%d), got alpha=%d", discSolidX, discY, got)
	}

	// Center hole inside elliptical disc should be cut out (alpha == 0) via even-odd rule.
	holeX := int(math.Round(float64(W) * (72.8 / logoViewWidth)))
	if got := mask.Sample(holeX, discY); got != 0 {
		t.Fatalf("expected transparent disc center hole at (%d,%d), got alpha=%d", holeX, discY, got)
	}

	// VIDEO label region (bottom 15% of mask) should contain opaque pixels when includeVideo=true.
	videoPixels := 0
	for y := int(math.Round(float64(H) * 0.83)); y < H; y++ {
		for x := 0; x < W; x++ {
			if mask.Sample(x, y) > 128 {
				videoPixels++
			}
		}
	}
	if videoPixels < 50 {
		t.Fatalf("expected VIDEO sub-label pixels when includeVideo=true, got %d", videoPixels)
	}

	// With includeVideo=false, top letters and disc still render cleanly.
	maskNoVideo := RasterizeDVDMask(320, 146, false)
	discYNoVid := int(math.Round(146.0 * (61.5 / logoViewHeightNoText)))
	if got := maskNoVideo.Sample(holeX, discYNoVid); got != 0 {
		t.Fatalf("expected transparent disc hole in no-video mask, got alpha=%d", got)
	}
}

func TestKittyRendererChunkingAndPlacements(t *testing.T) {
	var out bytes.Buffer
	kr := NewKittyRenderer(&out)

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
	if !strings.Contains(uploadStream, "\x1b_Ga=t,f=100,i=100,q=2,m=") {
		t.Fatalf("expected PNG transmission header for image id 100")
	}

	// Verify every APC payload chunk is <= 4096 bytes.
	parts := strings.Split(uploadStream, "\x1b_G")
	for _, part := range parts[1:] {
		end := strings.Index(part, "\x1b\\")
		if end < 0 {
			t.Fatalf("unterminated APC sequence in upload stream")
		}
		semi := strings.IndexByte(part[:end], ';')
		if semi >= 0 {
			payloadLen := end - (semi + 1)
			if payloadLen > 4096 {
				t.Fatalf("APC base64 chunk exceeded 4096 bytes: %d", payloadLen)
			}
		}
	}

	// Geometry invariants: logoW=240, cellW=10 -> logoCols=24, placeCols=25, cropW=250, marginX=10.
	// logoH=135, cellH=20 -> logoRows=7, placeRows=8, cropH=160, marginY=25.
	// Frame 1: place color 0 (id=100) at pixel (27, 45).
	// Expected: col0=2 -> ANSI col 3, subX=7 -> srcX=10-7=3; row0=2 -> ANSI row 3, subY=5 -> srcY=25-5=20.
	out.Reset()
	if err := kr.DrawFrame(27, 45, 100, 40, 0, false, ""); err != nil {
		t.Fatalf("DrawFrame failed: %v", err)
	}
	frame1 := out.String()
	if strings.Contains(frame1, "a=d") {
		t.Fatalf("first frame should not delete an existing placement: %q", frame1)
	}
	if strings.Contains(frame1, "X=") || strings.Contains(frame1, "Y=") {
		t.Fatalf("expected fixed-box crop placement without X=/Y= cell offsets: %q", frame1)
	}
	wantPut1 := "\x1b[3;3H\x1b_Ga=p,i=100,p=1,x=3,y=20,w=250,h=160,c=25,r=8,C=1,q=2\x1b\\"
	if !strings.Contains(frame1, wantPut1) {
		t.Fatalf("unexpected frame1 placement:\ngot:  %q\nwant: %q", frame1, wantPut1)
	}

	// Frame 2: same color (id=100) moved 3px right across cell boundary to (30, 46).
	// Expected: col0=3 -> ANSI col 4, subX=0 -> srcX=10; row0=2 -> ANSI row 3, subY=6 -> srcY=19; c=25,r=8 unchanged.
	out.Reset()
	_ = kr.DrawFrame(30, 46, 100, 40, 0, false, "")
	frame2 := out.String()
	if strings.Contains(frame2, "a=d") {
		t.Fatalf("moving same image id should update placement p=1 in place without a=d: %q", frame2)
	}
	wantPut2 := "\x1b[3;4H\x1b_Ga=p,i=100,p=1,x=10,y=19,w=250,h=160,c=25,r=8,C=1,q=2\x1b\\"
	if !strings.Contains(frame2, wantPut2) {
		t.Fatalf("unexpected frame2 placement:\ngot:  %q\nwant: %q", frame2, wantPut2)
	}

	// Frame 3: bounce changes color to colorIdx=1 (id=101) at exact bottom-right wall (px=760, py=665).
	// Expected: maxCol0 = 100-25 = 75 -> ANSI col 76, subX = 760-750 = 10 -> srcX = 0;
	//           maxRow0 = 40-8 = 32   -> ANSI row 33, subY = 665-640 = 25 -> srcY = 0;
	//           col0 + c = 75 + 25 = 100 <= 100 (never overflows grid!).
	out.Reset()
	_ = kr.DrawFrame(760, 665, 100, 40, 1, false, "")
	frame3 := out.String()
	wantDel := "\x1b_Ga=d,d=i,i=100,p=1,q=2\x1b\\"
	wantPut3 := "\x1b[33;76H\x1b_Ga=p,i=101,p=1,x=0,y=0,w=250,h=160,c=25,r=8,C=1,q=2\x1b\\"
	if !strings.HasPrefix(frame3, "\x1b[?2026h") || !strings.HasSuffix(frame3, "\x1b[?2026l") {
		t.Fatalf("expected synchronized output wrapper on frame3: %q", frame3)
	}
	if !strings.Contains(frame3, wantDel) || !strings.Contains(frame3, wantPut3) {
		t.Fatalf("expected atomic delete+put on color change at bottom-right wall:\ngot:  %q\nwant: %q", frame3, wantPut3)
	}
}

func TestPhysicsAimAndPredictCornerAgreement(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		p := NewPhysics(1200, 720, 320, 8)
		p.X = 150 + float64(trial*37%800)
		p.Y = 110 + float64(trial*53%450)
		if trial%2 == 0 {
			p.VX = -math.Abs(p.VX)
		}
		if trial%3 == 0 {
			p.VY = -math.Abs(p.VY)
		}

		p.AimForCorner()
		predSec, predCorner, ok := p.PredictNextCorner(50)
		if !ok {
			t.Fatalf("trial %d: expected PredictNextCorner to find upcoming corner after AimForCorner", trial)
		}

		// Simulate at 120 FPS and verify the corner hit happens at predicted time & corner.
		const dt = 1.0 / 120.0
		maxSteps := int(math.Ceil((predSec+1.0)/dt)) + 10
		hitAt := -1.0
		var hitCorner string
		prevColor := p.ColorIdx

		for step := 1; step <= maxSteps; step++ {
			ev := p.Step(dt)
			if ev.BouncedWall {
				if p.ColorIdx == prevColor {
					t.Fatalf("expected color to change on bounce, stayed at %d", p.ColorIdx)
				}
				prevColor = p.ColorIdx
			}
			if ev.HitCorner {
				hitAt = float64(step) * dt
				hitCorner = ev.CornerName
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
}

func TestInputParserHandlesEscapeResponsesAndKeys(t *testing.T) {
	ch := make(chan InputEvent, 16)

	// Concatenated Kitty OK + CSI 16t cell pixel reply + DA1 reply + 'c' + Up arrow.
	packet := []byte("\x1b_Gi=31;OK\x1b\\\x1b[6;34;15t\x1b[?62;22cc\x1b[A")
	parseInputBytes(packet, ch)

	if len(ch) != 3 {
		t.Fatalf("expected 3 parsed events (cellSize, 'c', Up), got %d", len(ch))
	}
	ev1 := <-ch
	if ev1.CellW != 15 || ev1.CellH != 34 {
		t.Fatalf("expected CSI 16t cell size 15x34, got %dx%d", ev1.CellW, ev1.CellH)
	}
	ev2 := <-ch
	if ev2.Action != ActionAimCorner {
		t.Fatalf("expected ActionAimCorner, got %v", ev2.Action)
	}
	ev3 := <-ch
	if ev3.Action != ActionSpeedUp {
		t.Fatalf("expected ActionSpeedUp, got %v", ev3.Action)
	}

	// Standalone Esc byte triggers quit.
	parseInputBytes([]byte{0x1b}, ch)
	evEsc := <-ch
	if evEsc.Action != ActionQuit {
		t.Fatalf("expected standalone Esc to trigger ActionQuit, got %v", evEsc.Action)
	}
}
