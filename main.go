package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

const defaultVersion = "v0.6.0"

var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return defaultVersion
}

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
	ShowVersion  bool
	PreviewPath  string
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.ModeFlag, "mode", "auto", "render mode: auto, kitty, ansi, or ascii")
	flag.IntVar(&cfg.FPS, "fps", 240, "target frames per second (15..360)")
	flag.Float64Var(&cfg.SpeedMult, "speed", 1.0, "movement speed multiplier (e.g. 0.5, 1.0, 2.0)")
	flag.Float64Var(&cfg.Scale, "scale", 0.21, "logo width as fraction of screen width (0.06..0.60)")
	flag.BoolVar(&cfg.ShowStats, "stats", false, "show bottom status bar (bounces, corners, next corner ETA)")
	flag.BoolVar(&cfg.Screensaver, "screensaver", false, "exit immediately on any keypress or mouse activity")
	flag.BoolVar(&cfg.Screensaver, "x", false, "shorthand for -screensaver (exit on key/mouse)")
	flag.BoolVar(&cfg.NoVideoLabel, "no-video", false, "omit 'VIDEO' label under the DVD disc")
	flag.BoolVar(&cfg.BlackBG, "black", false, "force opaque pitch-black background instead of terminal default")
	flag.BoolVar(&cfg.ShowVersion, "version", false, "print version and exit")
	flag.BoolVar(&cfg.ShowVersion, "v", false, "shorthand for -version")
	flag.StringVar(&cfg.PreviewPath, "preview", "", "render a PNG preview sheet of the vector DVD logo to path and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "dvd %s — CLI DVD Video screensaver (Kitty Graphics + ANSI/ASCII fallback)\n\n", buildVersion())
		fmt.Fprintf(os.Stderr, "Usage:\n  dvd [flags]\n\nFlags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nInteractive Keys:\n")
		fmt.Fprintf(os.Stderr, "  q / Esc / Ctrl+C   Quit\n")
		fmt.Fprintf(os.Stderr, "  s / ?              Toggle stats HUD & next-corner countdown\n")
		fmt.Fprintf(os.Stderr, "  c                  Subtly line up an upcoming corner hit\n")
		fmt.Fprintf(os.Stderr, "  r                  Randomize position, angle, and color\n")
		fmt.Fprintf(os.Stderr, "  + / -  (Up / Down) Increase / decrease bounce speed\n")
		fmt.Fprintf(os.Stderr, "  [ / ]              Shrink / grow logo size\n")
		fmt.Fprintf(os.Stderr, "  n                  Cycle logo color manually\n")
		fmt.Fprintf(os.Stderr, "  m                  Switch render mode (kitty -> ansi -> ascii)\n")
		fmt.Fprintf(os.Stderr, "  Ctrl+L             Force full screen refresh\n")
		fmt.Fprintf(os.Stderr, "  Ctrl+Z             Suspend to shell (resume with fg)\n")
		fmt.Fprintf(os.Stderr, "  Space              Pause / resume\n")
	}
	flag.Parse()

	if cfg.ShowVersion {
		fmt.Printf("dvd %s\n", buildVersion())
		return
	}

	if cfg.PreviewPath != "" {
		if err := writePreviewSheet(cfg.PreviewPath, !cfg.NoVideoLabel); err != nil {
			fmt.Fprintf(os.Stderr, "dvd: error writing preview: %v\n", err)
			os.Exit(1)
		}
		return
	}

	cfg.FPS = min(max(15, cfg.FPS), 360)
	cfg.Scale = clamp(cfg.Scale, 0.06, 0.60)
	cfg.SpeedMult = clamp(cfg.SpeedMult, 0.05, 10.0)

	if err := runScreensaver(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "dvd: %v\n", err)
		os.Exit(1)
	}
}

// framePacer converts noisy OS timer wakeups into steady animation timesteps while
// tracking cumulative phase debt so transient scheduler jitter neither stutters nor loses speed,
// and steady terminal VSync backpressure (e.g. 60/120 Hz) locks smoothly without drift.
type framePacer struct {
	targetDT   float64
	smoothedDT float64
	debt       float64
}

func newFramePacer(fps int) framePacer {
	dt := 1.0 / float64(max(1, fps))
	return framePacer{
		targetDT:   dt,
		smoothedDT: dt,
	}
}

func (fp *framePacer) Reset() {
	fp.smoothedDT = fp.targetDT
	fp.debt = 0
}

func (fp *framePacer) NextDT(rawDT float64) float64 {
	if rawDT <= 0 {
		return 0
	}
	// Cap long stalls (workspace switch, window-drag freeze) so the logo never teleports.
	if rawDT > 0.08 {
		fp.debt = 0
		return fp.smoothedDT
	}

	sample := clamp(rawDT, 0.25*fp.targetDT, 0.05)
	fp.smoothedDT += (sample - fp.smoothedDT) * 0.12

	fp.debt += rawDT - fp.smoothedDT
	maxDebt := 2.5 * fp.smoothedDT
	fp.debt = clamp(fp.debt, -maxDebt, maxDebt)

	corr := clamp(fp.debt*0.25, -0.25*fp.smoothedDT, 0.25*fp.smoothedDT)
	fp.debt -= corr
	return clamp(fp.smoothedDT+corr, 0.5*fp.smoothedDT, 1.5*fp.smoothedDT)
}

func drainTicker(t *time.Ticker) {
	select {
	case <-t.C:
	default:
	}
}

func runScreensaver(cfg Config) error {
	ttyIn := os.Stdin
	ttyOut := os.Stdout
	inOK := isConsoleInput(int(ttyIn.Fd()))
	_, _, _, _, outErr := getWinsize(int(ttyOut.Fd()))
	if !inOK || outErr != nil {
		inF, outF, closeTTY, openErr := openControllingTTY()
		if openErr != nil {
			if outErr != nil {
				return fmt.Errorf("not connected to an interactive terminal (%v)", outErr)
			}
			return fmt.Errorf("not connected to an interactive terminal (%v)", openErr)
		}
		defer closeTTY()
		if !inOK {
			ttyIn = inF
		}
		if outErr != nil {
			ttyOut = outF
		}
	}

	rawState, err := enableRawMode(int(ttyIn.Fd()), int(ttyOut.Fd()))
	if err != nil {
		return fmt.Errorf("failed to enable raw terminal mode: %w", err)
	}

	// Probe Kitty Graphics support + exact cell pixel size BEFORE starting the input reader goroutine.
	kittySupported, tmuxPassthrough, csiCellW, csiCellH := ProbeCapabilities(ttyIn, ttyOut)

	noColor := os.Getenv("NO_COLOR") != ""
	mode, err := resolveInitialMode(cfg.ModeFlag, kittySupported, noColor)
	if err != nil {
		rawState.Restore()
		return err
	}

	kitty := NewKittyRenderer(ttyOut, cfg.BlackBG, tmuxPassthrough)
	ansi := NewANSIRenderer(ttyOut, mode == ModeASCII, cfg.BlackBG, noColor)

	// Enter alternate screen (?1049h), hide cursor (?25l), and disable auto-wrap (?7l)
	// so edge-cell draws during live window shrinking never scroll the viewport.
	buildEnterSeq := func() string {
		seq := "\x1b[?1049h\x1b[?25l\x1b[?7l"
		if cfg.Screensaver {
			seq += "\x1b[?1003h\x1b[?1006h"
		}
		if cfg.BlackBG && !noColor {
			seq += "\x1b[48;2;0;0;0m"
		} else {
			seq += "\x1b[0m"
		}
		return seq + "\x1b[2J\x1b[H"
	}

	leaveTerminal := func() {
		_, _ = ttyOut.WriteString("\x1b[?2026l")
		if cfg.Screensaver {
			_, _ = ttyOut.WriteString("\x1b[?1003l\x1b[?1006l")
		}
		if kittySupported || mode == ModeKitty {
			kitty.ClearAll()
		}
		_, _ = ttyOut.WriteString("\x1b[0m\x1b[?7h\x1b[?25h\x1b[?1049l")
		rawState.Restore()
	}

	cleanedUp := false
	cleanup := func() {
		if cleanedUp {
			return
		}
		cleanedUp = true
		leaveTerminal()
	}
	defer cleanup()

	if _, err := ttyOut.WriteString(buildEnterSeq()); err != nil {
		return err
	}

	showStats := cfg.ShowStats
	metrics := QueryMetrics(int(ttyOut.Fd()), csiCellW, csiCellH)
	includeVideo := !cfg.NoVideoLabel

	logoPxW, logoPxH, maxX, maxY := computeArenaAndUpload(
		mode, metrics, cfg.Scale, includeVideo, showStats, kitty, ansi,
	)
	baseSpeed := computePixelSpeed(metrics, cfg.SpeedMult)
	phys := NewPhysics(maxX, maxY, baseSpeed, len(dvdPalette))

	sigExit, sigWinch, sigCont, stopSignals := setupSignals()
	defer stopSignals()

	inputCh := make(chan InputEvent, 64)
	StartInputReader(ttyIn, inputCh)

	paused := false
	frameDur := time.Second / time.Duration(cfg.FPS)
	pacer := newFramePacer(cfg.FPS)
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	startTime := time.Now()
	lastTick := startTime
	var hasMousePos bool
	var lastMouseCol, lastMouseRow int
	var hudState hudBuilder
	var lastETACalc time.Time
	consecutiveWriteErrs := 0

	usableRowsFor := func(rows int) int {
		if showStats && rows >= 8 {
			return rows - 1
		}
		return rows
	}

	arenaBounds := func() (float64, float64) {
		uRows := usableRowsFor(metrics.Rows)
		if mode == ModeKitty {
			cw := max(1, kitty.cellW)
			ch := max(1, kitty.cellH)
			return math.Max(1, float64(metrics.Cols*cw-logoPxW)),
				math.Max(1, float64(uRows*ch-logoPxH))
		}
		lw, lh := ansi.LogoCells()
		return math.Max(1, float64(max(1, metrics.Cols-lw)*metrics.CellW)),
			math.Max(1, float64(max(1, uRows-lh)*metrics.CellH))
	}

	renderFrame := func(now time.Time) bool {
		var hud string
		if showStats {
			if now.Sub(lastETACalc) > 250*time.Millisecond {
				hudState.RefreshETA(phys)
				lastETACalc = now
			}
			hud = hudState.Build(phys, mode, cfg.SpeedMult, paused)
		}

		colorIdx, flash := phys.ActiveDisplayColor()
		var werr error
		switch mode {
		case ModeKitty:
			werr = kitty.DrawFrame(phys.X, phys.Y, metrics.Cols, metrics.Rows, colorIdx, flash, hud)
		case ModeANSI, ModeASCII:
			col := dvdPalette[colorIdx%len(dvdPalette)]
			if flash {
				col = cornerFlashColor
			}
			uRows := usableRowsFor(metrics.Rows)
			lw, lh := ansi.LogoCells()
			colF := clamp(phys.X/float64(max(1, metrics.CellW)), 0, float64(max(0, metrics.Cols-lw)))
			rowF := clamp(phys.Y/float64(max(1, metrics.CellH)), 0, float64(max(0, uRows-lh)))
			werr = ansi.DrawFrame(colF, rowF, metrics.Cols, metrics.Rows, col, hud)
		}
		if werr != nil {
			consecutiveWriteErrs++
			if consecutiveWriteErrs >= 3 {
				return false
			}
		} else {
			consecutiveWriteErrs = 0
		}
		return true
	}

	clearHUD := func() {
		if mode == ModeKitty {
			kitty.ClearHUDLine(metrics.Rows)
		} else {
			ansi.ClearHUDLine(metrics.Rows)
		}
	}

	reconfigure := func(clearScreen bool) bool {
		if clearScreen {
			// Begin synchronized output BEFORE clearing so resize/mode-switch never flashes a blank frame.
			bg := "\x1b[?2026h\x1b[0m"
			if cfg.BlackBG && !noColor {
				bg = "\x1b[?2026h\x1b[48;2;0;0;0m"
			}
			if mode != ModeKitty && kittySupported {
				kitty.ClearAll()
			}
			_, _ = ttyOut.WriteString(bg + "\x1b[2J\x1b[H")
			ansi.ResetPrev()
		}
		logoPxW, logoPxH, maxX, maxY = computeArenaAndUpload(
			mode, metrics, cfg.Scale, includeVideo, showStats, kitty, ansi,
		)
		phys.UpdateBounds(maxX, maxY)
		phys.SetSpeed(computePixelSpeed(metrics, cfg.SpeedMult))
		lastETACalc = time.Time{}
		now := time.Now()
		ok := renderFrame(now)
		if clearScreen {
			_, _ = ttyOut.WriteString("\x1b[?2026l")
		}
		drainTicker(ticker)
		lastTick = time.Now()
		pacer.Reset()
		return ok
	}

	resumeTerminal := func() bool {
		if r, err := enableRawMode(int(ttyIn.Fd()), int(ttyOut.Fd())); err == nil {
			rawState = r
		}
		_, _ = ttyOut.WriteString(buildEnterSeq())
		metrics = QueryMetrics(int(ttyOut.Fd()), csiCellW, csiCellH)
		return reconfigure(true)
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
	if !renderFrame(lastTick) {
		return nil
	}

	handleEvent := func(ev InputEvent) (exit bool, dirty bool) {
		if ev.CellW > 0 && ev.CellH > 0 {
			csiCellW, csiCellH = ev.CellW, ev.CellH
			updated := QueryMetrics(int(ttyOut.Fd()), csiCellW, csiCellH)
			if updated != metrics {
				metrics = updated
				scheduleReconfigure()
			}
			return false, false
		}

		if ev.Action == ActionMouse {
			if !cfg.Screensaver {
				return false, false
			}
			if ev.MouseMotion {
				if !hasMousePos {
					hasMousePos = true
					lastMouseCol, lastMouseRow = ev.MouseCol, ev.MouseRow
					return false, false
				}
				if ev.MouseCol == lastMouseCol && ev.MouseRow == lastMouseRow {
					return false, false
				}
				lastMouseCol, lastMouseRow = ev.MouseCol, ev.MouseRow
				if time.Since(startTime) >= 200*time.Millisecond {
					return true, false
				}
				return false, false
			}
			if !ev.MouseRelease && time.Since(startTime) >= 200*time.Millisecond {
				return true, false
			}
			return false, false
		}

		if cfg.Screensaver && ev.Action != ActionNone {
			return true, false
		}

		switch ev.Action {
		case ActionQuit:
			return true, false
		case ActionSuspend:
			if resizeTimer != nil {
				resizeTimer.Stop()
				resizeCh = nil
			}
			select {
			case <-sigCont:
			default:
			}
			leaveTerminal()
			if suspendProcess() {
				select {
				case <-sigCont:
					if !resumeTerminal() {
						return true, false
					}
				case <-sigExit:
					return true, false
				}
			} else if !resumeTerminal() {
				return true, false
			}
		case ActionRedraw:
			metrics = QueryMetrics(int(ttyOut.Fd()), csiCellW, csiCellH)
			if !reconfigure(true) {
				return true, false
			}
		case ActionToggleStats:
			showStats = !showStats
			maxX, maxY = arenaBounds()
			phys.UpdateBounds(maxX, maxY)
			lastETACalc = time.Time{}
			if !showStats {
				clearHUD()
			}
			return false, true
		case ActionTogglePause:
			paused = !paused
			drainTicker(ticker)
			lastTick = time.Now()
			pacer.Reset()
			return false, true
		case ActionAimCorner:
			phys.AimForCorner()
			lastETACalc = time.Time{}
			if showStats {
				return false, true
			}
		case ActionRandomize:
			phys.Randomize()
			lastETACalc = time.Time{}
			return false, true
		case ActionSpeedUp:
			cfg.SpeedMult = math.Min(8.0, cfg.SpeedMult*1.2)
			phys.SetSpeed(computePixelSpeed(metrics, cfg.SpeedMult))
			lastETACalc = time.Time{}
			if showStats {
				return false, true
			}
		case ActionSpeedDown:
			cfg.SpeedMult = math.Max(0.15, cfg.SpeedMult/1.2)
			phys.SetSpeed(computePixelSpeed(metrics, cfg.SpeedMult))
			lastETACalc = time.Time{}
			if showStats {
				return false, true
			}
		case ActionSizeUp:
			cfg.Scale = math.Min(0.55, cfg.Scale*1.12)
			if !reconfigure(mode != ModeKitty) {
				return true, false
			}
		case ActionSizeDown:
			cfg.Scale = math.Max(0.07, cfg.Scale/1.12)
			if !reconfigure(mode != ModeKitty) {
				return true, false
			}
		case ActionNextColor:
			phys.ColorIdx = (phys.ColorIdx + 1) % len(dvdPalette)
			return false, true
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
			if !reconfigure(true) {
				return true, false
			}
		}
		return false, false
	}

	for {
		select {
		case <-sigExit:
			return nil

		case <-sigCont:
			if !resumeTerminal() {
				return nil
			}

		case <-sigWinch:
			// Coalesce burst SIGWINCH deliveries during rapid live window drag.
		drainWinch:
			for {
				select {
				case <-sigWinch:
				default:
					break drainWinch
				}
			}
			_, _ = ttyOut.WriteString("\x1b[16t")
			metrics = QueryMetrics(int(ttyOut.Fd()), 0, 0)
			if !metrics.HasPx && csiCellW > 0 && csiCellH > 0 {
				metrics = QueryMetrics(int(ttyOut.Fd()), csiCellW, csiCellH)
			}
			if mode == ModeKitty {
				newMaxX, newMaxY := arenaBounds()
				phys.UpdateBounds(newMaxX, newMaxY)
				phys.SetSpeed(computePixelSpeed(metrics, cfg.SpeedMult))
				scheduleReconfigure()
			} else if !reconfigure(true) {
				return nil
			}

		case <-resizeCh:
			resizeCh = nil
			if !reconfigure(true) {
				return nil
			}

		case ev := <-inputCh:
			exit, dirty := handleEvent(ev)
			if exit {
				return nil
			}
			// Coalesce any simultaneously queued input events before rendering once.
		drainInputs:
			for {
				select {
				case nextEv := <-inputCh:
					ex, d := handleEvent(nextEv)
					if ex {
						return nil
					}
					dirty = dirty || d
				default:
					break drainInputs
				}
			}
			if dirty && paused {
				if !renderFrame(time.Now()) {
					return nil
				}
			}

		case <-ticker.C:
			// Measure actual monotonic wall time now (not potentially queued channel send time).
			now := time.Now()
			rawDT := now.Sub(lastTick).Seconds()
			lastTick = now

			if !paused {
				stepEv := phys.Step(pacer.NextDT(rawDT))
				if stepEv.BouncedWall {
					lastETACalc = time.Time{}
				}
			}

			if !renderFrame(now) {
				return nil
			}
		}
	}
}

func resolveInitialMode(flagVal string, kittySupported, noColor bool) (RenderMode, error) {
	switch strings.ToLower(strings.TrimSpace(flagVal)) {
	case "auto", "":
		if kittySupported {
			return ModeKitty, nil
		}
		if noColor {
			return ModeASCII, nil
		}
		return ModeANSI, nil
	case "kitty", "graphics":
		return ModeKitty, nil
	case "ansi", "halfblock", "truecolor":
		return ModeANSI, nil
	case "ascii", "text":
		return ModeASCII, nil
	default:
		return ModeANSI, fmt.Errorf("unknown -mode %q (expected auto, kitty, ansi, or ascii)", flagVal)
	}
}

func computeArenaAndUpload(
	mode RenderMode,
	m TermMetrics,
	scale float64,
	includeVideo bool,
	showStats bool,
	kitty *KittyRenderer,
	ansi *ANSIRenderer,
) (logoPxW, logoPxH int, maxX, maxY float64) {
	aspect := LogoAspectRatio(includeVideo)
	gridW := m.GridPxW()
	gridH := m.GridPxH()
	uRows := m.Rows
	if showStats && uRows >= 8 {
		uRows--
	}

	if mode == ModeKitty {
		logoPxW = int(math.Round(float64(gridW) * scale))
		logoPxH = int(math.Round(float64(logoPxW) / aspect))
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
		maxY = math.Max(1, float64(uRows*m.CellH-logoPxH))
		return logoPxW, logoPxH, maxX, maxY
	}

	colsW := int(math.Round(float64(m.Cols) * scale))
	if colsW < 14 {
		colsW = min(max(4, m.Cols-2), 14)
	}
	cellAspect := float64(m.CellH) / float64(max(1, m.CellW))
	if cellAspect < 1.2 || cellAspect > 3.2 {
		cellAspect = 2.0
	}
	rowsH := max(4, int(math.Round(float64(colsW)/(aspect*cellAspect))))
	ansi.ResizeLogo(colsW, rowsH, includeVideo)
	lw, lh := ansi.LogoCells()
	maxX = math.Max(1, float64(max(1, m.Cols-lw)*m.CellW))
	maxY = math.Max(1, float64(max(1, uRows-lh)*m.CellH))
	return lw * m.CellW, lh * m.CellH, maxX, maxY
}

func computePixelSpeed(m TermMetrics, mult float64) float64 {
	diag := math.Hypot(float64(m.GridPxW()), float64(m.GridPxH()))
	return diag * 0.185 * mult
}

// hudBuilder formats the status bar into double-buffered inline storage so even at 240 FPS
// with -stats active, HUD updates incur 0 heap allocations and never alias renderer prevHUD state.
type hudBuilder struct {
	bufs      [2][384]byte
	etaBuf    [80]byte
	etaLen    int
	cur       int
	hasCached bool
	bounces   int
	corners   int
	mode      RenderMode
	speedX10  int
	paused    bool
	lastNote  string
	cached    string
}

func appendNextCornerETA(dst []byte, p *Physics) []byte {
	secs, cname, ok := p.PredictNextCorner(6000)
	if !ok {
		return append(dst, "next corner: >30m (press 'c' to nudge)"...)
	}
	dst = append(dst, "next corner: "...)
	if secs < 60 {
		dst = strconv.AppendFloat(dst, secs, 'f', 1, 64)
		dst = append(dst, "s ("...)
	} else {
		totalSec := int(math.Round(secs))
		mins := totalSec / 60
		rem := totalSec % 60
		dst = strconv.AppendInt(dst, int64(mins), 10)
		dst = append(dst, 'm')
		if rem < 10 {
			dst = append(dst, '0')
		}
		dst = strconv.AppendInt(dst, int64(rem), 10)
		dst = append(dst, "s ("...)
	}
	dst = append(dst, cname...)
	return append(dst, ')')
}

func formatNextCornerETA(p *Physics) string {
	var tmp [80]byte
	return string(appendNextCornerETA(tmp[:0], p))
}

func (h *hudBuilder) RefreshETA(p *Physics) {
	var tmp [80]byte
	next := appendNextCornerETA(tmp[:0], p)
	if !bytes.Equal(h.etaBuf[:h.etaLen], next) {
		h.etaLen = copy(h.etaBuf[:], next)
		h.hasCached = false
	}
}

func (h *hudBuilder) Build(p *Physics, mode RenderMode, speedMult float64, paused bool) string {
	if h.etaLen == 0 {
		h.RefreshETA(p)
	}
	speedX10 := int(math.Round(speedMult * 10.0))
	if h.hasCached &&
		h.bounces == p.Bounces &&
		h.corners == p.Corners &&
		h.mode == mode &&
		h.speedX10 == speedX10 &&
		h.paused == paused &&
		h.lastNote == p.LastNote {
		return h.cached
	}

	h.cur ^= 1
	b := h.bufs[h.cur][:0]
	b = append(b, "bounces: "...)
	b = strconv.AppendInt(b, int64(p.Bounces), 10)
	b = append(b, "  •  corners: "...)
	b = strconv.AppendInt(b, int64(p.Corners), 10)
	b = append(b, "  •  "...)
	b = append(b, h.etaBuf[:h.etaLen]...)
	b = append(b, "  •  mode: "...)
	b = append(b, mode.String()...)
	b = append(b, " ("...)
	b = strconv.AppendInt(b, int64(speedX10/10), 10)
	b = append(b, '.', byte('0'+speedX10%10), 'x', ')')
	if paused {
		b = append(b, "  •  PAUSED"...)
	}
	if p.LastNote != "" {
		note := p.LastNote
		if len(note) > 120 {
			note = note[:120]
		}
		b = append(b, "  •  "...)
		b = append(b, note...)
	}

	h.bounces = p.Bounces
	h.corners = p.Corners
	h.mode = mode
	h.speedX10 = speedX10
	h.paused = paused
	h.lastNote = p.LastNote
	h.hasCached = true
	h.cached = unsafe.String(&h.bufs[h.cur][0], len(b))
	return h.cached
}

// writePreviewSheet renders a dark terminal showcase image featuring multiple palette-colored
// vector DVD logos so visual fidelity and anti-aliasing can be inspected directly.
func writePreviewSheet(outPath string, includeVideo bool) error {
	const W, H = 960, 540
	canvas := image.NewNRGBA(image.Rect(0, 0, W, H))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{color.RGBA{0x14, 0x16, 0x1B, 0xFF}}, image.Point{}, draw.Src)

	aspect := LogoAspectRatio(includeVideo)
	heroW := 420
	heroH := int(math.Round(float64(heroW) / aspect))
	heroMask := RasterizeDVDMask(heroW, heroH, includeVideo)
	heroImg := heroMask.TintRGBA(dvdPalette[0])
	draw.Draw(canvas, image.Rect((W-heroW)/2, 65, (W+heroW)/2, 65+heroH), heroImg, image.Point{}, draw.Over)

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
	if err := png.Encode(f, canvas); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
