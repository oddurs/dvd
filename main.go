package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type RenderMode int

const (
	ModeKitty RenderMode = iota
	ModeANSI
	ModeASCII
)

func (m RenderMode) String() string {
	switch m {
	case ModeKitty:
		return "kitty-graphics"
	case ModeANSI:
		return "ansi-halfblock"
	default:
		return "ascii"
	}
}

type Config struct {
	ModeFlag     string
	FPS          int
	SpeedMult    float64
	Scale        float64
	ShowStats    bool
	Screensaver  bool
	NoVideoLabel bool
	BlackBG      bool
	PreviewPath  string
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.ModeFlag, "mode", "auto", "render mode: auto, kitty, ansi, or ascii")
	flag.IntVar(&cfg.FPS, "fps", 60, "target frames per second (15..240)")
	flag.Float64Var(&cfg.SpeedMult, "speed", 1.0, "movement speed multiplier (e.g. 0.5, 1.0, 2.0)")
	flag.Float64Var(&cfg.Scale, "scale", 0.21, "logo width as fraction of screen width (0.06..0.60)")
	flag.BoolVar(&cfg.ShowStats, "stats", false, "show subtle bottom status bar (bounces, corners, next corner ETA)")
	flag.BoolVar(&cfg.Screensaver, "screensaver", false, "exit immediately on any keypress")
	flag.BoolVar(&cfg.Screensaver, "x", false, "shorthand for --screensaver (exit on any key)")
	flag.BoolVar(&cfg.NoVideoLabel, "no-video", false, "omit 'VIDEO' label under the DVD disc")
	flag.BoolVar(&cfg.BlackBG, "black", false, "force opaque pitch-black background instead of terminal default")
	flag.StringVar(&cfg.PreviewPath, "preview", "", "render a PNG preview of the vector DVD logo to path and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "dvd — CLI DVD Player Screensaver (with native Ghostty / Kitty Graphics support)\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n  dvd [flags]\n\nFlags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nInteractive Keys:\n")
		fmt.Fprintf(os.Stderr, "  q / Esc / Ctrl+C   Quit\n")
		fmt.Fprintf(os.Stderr, "  s / ?              Toggle stats HUD & next-corner countdown\n")
		fmt.Fprintf(os.Stderr, "  c                  Subtly line up an upcoming corner hit\n")
		fmt.Fprintf(os.Stderr, "  + / -  (Up / Down) Increase / decrease bounce speed\n")
		fmt.Fprintf(os.Stderr, "  [ / ]              Shrink / grow logo size\n")
		fmt.Fprintf(os.Stderr, "  n                  Cycle logo color manually\n")
		fmt.Fprintf(os.Stderr, "  m                  Switch render mode (kitty -> ansi -> ascii)\n")
		fmt.Fprintf(os.Stderr, "  Space              Pause / resume\n")
	}
	flag.Parse()

	if cfg.PreviewPath != "" {
		if err := writePreviewSheet(cfg.PreviewPath, !cfg.NoVideoLabel); err != nil {
			fmt.Fprintf(os.Stderr, "error writing preview: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if cfg.FPS < 15 {
		cfg.FPS = 15
	} else if cfg.FPS > 240 {
		cfg.FPS = 240
	}
	if cfg.Scale < 0.06 {
		cfg.Scale = 0.06
	} else if cfg.Scale > 0.60 {
		cfg.Scale = 0.60
	}
	if cfg.SpeedMult <= 0.05 {
		cfg.SpeedMult = 0.05
	}

	if err := runScreensaver(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "dvd: %v\n", err)
		os.Exit(1)
	}
}

func runScreensaver(cfg Config) error {
	ttyIn := os.Stdin
	ttyOut := os.Stdout
	var openedTTY *os.File
	if _, _, _, _, err := getWinsize(int(ttyOut.Fd())); err != nil {
		f, openErr := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if openErr != nil {
			return fmt.Errorf("not connected to an interactive terminal (%v)", err)
		}
		openedTTY = f
		ttyIn = f
		ttyOut = f
	}
	if openedTTY != nil {
		defer openedTTY.Close()
	}

	rawState, err := enableRawMode(int(ttyIn.Fd()))
	if err != nil {
		return fmt.Errorf("failed to enable raw terminal mode: %w", err)
	}

	// Probe Kitty Graphics support + exact cell pixel size BEFORE starting the input reader goroutine.
	kittySupported, csiCellW, csiCellH := ProbeCapabilities(ttyIn, ttyOut)

	mode, err := resolveInitialMode(cfg.ModeFlag, kittySupported)
	if err != nil {
		rawState.Restore()
		return err
	}

	kitty := NewKittyRenderer(ttyOut)
	ansi := NewANSIRenderer(ttyOut, mode == ModeASCII)

	cleanedUp := false
	cleanup := func() {
		if cleanedUp {
			return
		}
		cleanedUp = true
		if kittySupported || mode == ModeKitty {
			kitty.ClearAll()
		}
		_, _ = ttyOut.WriteString("\x1b[0m\x1b[?25h\x1b[?1049l")
		rawState.Restore()
	}
	defer cleanup()

	// Enter alternate screen and hide cursor.
	enterSeq := "\x1b[?1049h\x1b[?25l"
	if cfg.BlackBG {
		enterSeq += "\x1b[48;2;0;0;0m"
	} else {
		enterSeq += "\x1b[0m"
	}
	enterSeq += "\x1b[2J\x1b[H"
	_, _ = ttyOut.WriteString(enterSeq)

	metrics := QueryMetrics(int(ttyOut.Fd()), csiCellW, csiCellH)
	includeVideo := !cfg.NoVideoLabel

	logoPxW, logoPxH, maxX, maxY := computeArenaAndUpload(
		mode, metrics, cfg.Scale, includeVideo, kitty, ansi,
	)
	baseSpeed := computePixelSpeed(metrics, cfg.SpeedMult)
	phys := NewPhysics(maxX, maxY, baseSpeed, len(dvdPalette))

	// Set up signal handlers for clean exit and live terminal resize.
	sigExit := make(chan os.Signal, 2)
	signal.Notify(sigExit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	sigWinch := make(chan os.Signal, 2)
	signal.Notify(sigWinch, syscall.SIGWINCH)

	inputCh := make(chan InputEvent, 32)
	StartInputReader(ttyIn, inputCh)

	paused := false
	showStats := cfg.ShowStats
	frameDur := time.Second / time.Duration(cfg.FPS)
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	lastTick := time.Now()
	var cachedETA string
	var lastETACalc time.Time

	renderFrame := func(now time.Time) {
		var hud string
		if showStats {
			if now.Sub(lastETACalc) > 250*time.Millisecond {
				cachedETA = formatNextCornerETA(phys)
				lastETACalc = now
			}
			hud = buildHUD(phys, mode, cfg.SpeedMult, paused, cachedETA)
		}

		colorIdx, flash := phys.ActiveDisplayColor()
		switch mode {
		case ModeKitty:
			px := int(math.Round(phys.X))
			py := int(math.Round(phys.Y))
			_ = kitty.DrawFrame(px, py, metrics.Cols, metrics.Rows, colorIdx, flash, hud)
		case ModeANSI, ModeASCII:
			col := dvdPalette[colorIdx%len(dvdPalette)]
			if flash {
				col = cornerFlashColor
			}
			c0 := int(math.Round(phys.X / float64(metrics.CellW)))
			r0 := int(math.Round(phys.Y / float64(metrics.CellH)))
			lw, lh := ansi.LogoCells()
			if c0 > metrics.Cols-lw {
				c0 = max(0, metrics.Cols-lw)
			}
			if r0 > metrics.Rows-lh {
				r0 = max(0, metrics.Rows-lh)
			}
			_ = ansi.DrawFrame(c0, r0, metrics.Cols, metrics.Rows, col, hud)
		}
	}

	reconfigure := func(clearScreen bool) {
		if clearScreen {
			if mode != ModeKitty && kittySupported {
				kitty.ClearAll()
			}
			bg := "\x1b[0m"
			if cfg.BlackBG {
				bg = "\x1b[48;2;0;0;0m"
			}
			_, _ = ttyOut.WriteString(bg + "\x1b[2J\x1b[H")
			ansi.ResetPrev()
		}
		logoPxW, logoPxH, maxX, maxY = computeArenaAndUpload(
			mode, metrics, cfg.Scale, includeVideo, kitty, ansi,
		)
		phys.UpdateBounds(maxX, maxY)
		phys.SetSpeed(computePixelSpeed(metrics, cfg.SpeedMult))
		lastETACalc = time.Time{}
		renderFrame(time.Now())
	}

	// Debounce timer so live window drag-resizing stays silky smooth and re-rasterizes textures once settled.
	var resizeTimer *time.Timer
	var resizeCh <-chan time.Time
	scheduleReconfigure := func() {
		if resizeTimer != nil {
			resizeTimer.Stop()
		}
		resizeTimer = time.NewTimer(65 * time.Millisecond)
		resizeCh = resizeTimer.C
	}

	// Render initial frame immediately.
	renderFrame(lastTick)

	for {
		select {
		case <-sigExit:
			return nil

		case <-sigWinch:
			_, _ = ttyOut.WriteString("\x1b[16t")
			metrics = QueryMetrics(int(ttyOut.Fd()), 0, 0)
			if !metrics.HasPx && csiCellW > 0 && csiCellH > 0 {
				metrics = QueryMetrics(int(ttyOut.Fd()), csiCellW, csiCellH)
			}
			if mode == ModeKitty {
				// Keep existing cached textures bouncing inside updated bounds during drag; re-upload once settled.
				cw := max(1, kitty.cellW)
				ch := max(1, kitty.cellH)
				newMaxX := math.Max(1, float64(metrics.Cols*cw-logoPxW))
				newMaxY := math.Max(1, float64(metrics.Rows*ch-logoPxH))
				phys.UpdateBounds(newMaxX, newMaxY)
				phys.SetSpeed(computePixelSpeed(metrics, cfg.SpeedMult))
				scheduleReconfigure()
			} else {
				reconfigure(true)
			}

		case <-resizeCh:
			resizeCh = nil
			reconfigure(true)

		case ev := <-inputCh:
			if ev.CellW > 0 && ev.CellH > 0 {
				csiCellW, csiCellH = ev.CellW, ev.CellH
				updated := QueryMetrics(int(ttyOut.Fd()), csiCellW, csiCellH)
				if updated != metrics {
					metrics = updated
					scheduleReconfigure()
				}
				continue
			}

			if cfg.Screensaver && ev.Action != ActionNone {
				return nil
			}

			switch ev.Action {
			case ActionQuit:
				return nil
			case ActionToggleStats:
				showStats = !showStats
				lastETACalc = time.Time{}
				if !showStats {
					kitty.ClearHUDLine(metrics.Rows)
				}
				renderFrame(time.Now())
			case ActionTogglePause:
				paused = !paused
				lastTick = time.Now()
				renderFrame(lastTick)
			case ActionAimCorner:
				phys.AimForCorner()
				lastETACalc = time.Time{}
			case ActionSpeedUp:
				cfg.SpeedMult = math.Min(8.0, cfg.SpeedMult*1.2)
				phys.SetSpeed(computePixelSpeed(metrics, cfg.SpeedMult))
				lastETACalc = time.Time{}
			case ActionSpeedDown:
				cfg.SpeedMult = math.Max(0.15, cfg.SpeedMult/1.2)
				phys.SetSpeed(computePixelSpeed(metrics, cfg.SpeedMult))
				lastETACalc = time.Time{}
			case ActionSizeUp:
				cfg.Scale = math.Min(0.55, cfg.Scale*1.12)
				reconfigure(mode != ModeKitty)
			case ActionSizeDown:
				cfg.Scale = math.Max(0.07, cfg.Scale/1.12)
				reconfigure(mode != ModeKitty)
			case ActionNextColor:
				phys.ColorIdx = (phys.ColorIdx + 1) % len(dvdPalette)
				renderFrame(time.Now())
			case ActionCycleMode:
				switch mode {
				case ModeKitty:
					mode = ModeANSI
					ansi.asciiOnly = false
				case ModeANSI:
					mode = ModeASCII
					ansi.asciiOnly = true
				case ModeASCII:
					if kittySupported {
						mode = ModeKitty
					} else {
						mode = ModeANSI
						ansi.asciiOnly = false
					}
				}
				reconfigure(true)
			}

		case now := <-ticker.C:
			dt := now.Sub(lastTick).Seconds()
			lastTick = now

			if !paused {
				stepEv := phys.Step(dt)
				if stepEv.BouncedWall {
					lastETACalc = time.Time{}
				}
			}

			renderFrame(now)
		}
	}
}

func resolveInitialMode(flagVal string, kittySupported bool) (RenderMode, error) {
	switch strings.ToLower(strings.TrimSpace(flagVal)) {
	case "auto", "":
		if kittySupported {
			return ModeKitty, nil
		}
		return ModeANSI, nil
	case "kitty", "graphics":
		return ModeKitty, nil
	case "ansi", "halfblock", "truecolor":
		return ModeANSI, nil
	case "ascii", "text":
		return ModeASCII, nil
	default:
		return ModeANSI, fmt.Errorf("unknown --mode %q (expected auto, kitty, ansi, or ascii)", flagVal)
	}
}

func computeArenaAndUpload(
	mode RenderMode,
	m TermMetrics,
	scale float64,
	includeVideo bool,
	kitty *KittyRenderer,
	ansi *ANSIRenderer,
) (logoPxW, logoPxH int, maxX, maxY float64) {
	aspect := LogoAspectRatio(includeVideo)
	gridW := m.GridPxW()
	gridH := m.GridPxH()

	if mode == ModeKitty {
		logoPxW = int(math.Round(float64(gridW) * scale))
		logoPxH = int(math.Round(float64(logoPxW) / aspect))
		// Clamp so logo never exceeds 45% of screen height on very short wide splits.
		maxAllowedH := int(float64(gridH) * 0.45)
		if maxAllowedH >= 12 && logoPxH > maxAllowedH {
			logoPxH = maxAllowedH
			logoPxW = int(math.Round(float64(logoPxH) * aspect))
		}
		if logoPxW < 24 {
			logoPxW = min(gridW, 24)
		}
		if logoPxH < 12 {
			logoPxH = min(gridH, 12)
		}
		_ = kitty.UploadPalette(logoPxW, logoPxH, m.CellW, m.CellH, m.Cols, m.Rows, includeVideo, dvdPalette, cornerFlashColor)
		logoPxW = kitty.logoW
		logoPxH = kitty.logoH
		maxX = math.Max(1, float64(gridW-logoPxW))
		maxY = math.Max(1, float64(gridH-logoPxH))
		return logoPxW, logoPxH, maxX, maxY
	}

	// ANSI / ASCII cell-aligned bounds mapped onto virtual pixel coordinates.
	colsW := int(math.Round(float64(m.Cols) * scale))
	if colsW < 14 {
		colsW = min(m.Cols-2, 14)
	}
	cellAspect := float64(m.CellH) / float64(max(1, m.CellW))
	if cellAspect < 1.2 || cellAspect > 3.2 {
		cellAspect = 2.0
	}
	rowsH := int(math.Round(float64(colsW) / (aspect * cellAspect)))
	if rowsH < 4 {
		rowsH = 4
	}
	ansi.ResizeLogo(colsW, rowsH, includeVideo)
	lw, lh := ansi.LogoCells()
	maxX = math.Max(1, float64(max(1, m.Cols-lw)*m.CellW))
	maxY = math.Max(1, float64(max(1, m.Rows-lh)*m.CellH))
	return lw * m.CellW, lh * m.CellH, maxX, maxY
}

func computePixelSpeed(m TermMetrics, mult float64) float64 {
	diag := math.Hypot(float64(m.GridPxW()), float64(m.GridPxH()))
	// ~0.185 * viewport diagonal per second crosses a standard 16:10 window in ~5.5 seconds.
	return diag * 0.185 * mult
}

func formatNextCornerETA(p *Physics) string {
	secs, cname, ok := p.PredictNextCorner(6000)
	if !ok {
		return "next corner: >30m (press 'c' to nudge)"
	}
	if secs < 60 {
		return fmt.Sprintf("next corner: %.1fs (%s)", secs, cname)
	}
	mins := int(secs) / 60
	rem := int(math.Round(secs)) % 60
	return fmt.Sprintf("next corner: %dm%02ds (%s)", mins, rem, cname)
}

func buildHUD(p *Physics, mode RenderMode, speedMult float64, paused bool, eta string) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("bounces: %d", p.Bounces))
	parts = append(parts, fmt.Sprintf("corners: %d", p.Corners))
	parts = append(parts, eta)
	parts = append(parts, fmt.Sprintf("mode: %s (%.1fx)", mode.String(), speedMult))
	if paused {
		parts = append(parts, "PAUSED")
	}
	if p.LastNote != "" {
		parts = append(parts, p.LastNote)
	}
	return strings.Join(parts, "  •  ")
}

// writePreviewSheet renders a dark terminal showcase image featuring multiple palette-colored
// vector DVD logos so visual fidelity and anti-aliasing can be inspected directly.
func writePreviewSheet(outPath string, includeVideo bool) error {
	const W, H = 960, 540
	canvas := image.NewNRGBA(image.Rect(0, 0, W, H))
	// Fill with sleek Ghostty dark background (#14161B).
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{color.RGBA{0x14, 0x16, 0x1B, 0xFF}}, image.Point{}, draw.Src)

	// Draw subtle terminal border & corner highlight.
	aspect := LogoAspectRatio(includeVideo)
	heroW := 420
	heroH := int(math.Round(float64(heroW) / aspect))
	heroMask := RasterizeDVDMask(heroW, heroH, includeVideo)
	heroImg := heroMask.TintRGBA(dvdPalette[0])
	draw.Draw(canvas, image.Rect((W-heroW)/2, 65, (W+heroW)/2, 65+heroH), heroImg, image.Point{}, draw.Over)

	// Render smaller palette swatches along the bottom row.
	smallW := 180
	smallH := int(math.Round(float64(smallW) / aspect))
	smallMask := RasterizeDVDMask(smallW, smallH, includeVideo)
	positions := []struct {
		x, y int
		col  color.RGBA
	}{
		{50, 380, dvdPalette[1]},
		{280, 380, dvdPalette[2]},
		{510, 380, dvdPalette[3]},
		{730, 380, dvdPalette[4]},
	}
	for _, pos := range positions {
		swatch := smallMask.TintRGBA(pos.col)
		draw.Draw(canvas, image.Rect(pos.x, pos.y, pos.x+smallW, pos.y+smallH), swatch, image.Point{}, draw.Over)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, canvas)
}
