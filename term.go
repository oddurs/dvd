package main

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

// TermMetrics holds character-grid and physical-pixel dimensions of the terminal viewport.
type TermMetrics struct {
	Cols  int
	Rows  int
	CellW int
	CellH int
	HasPx bool
}

func (m TermMetrics) GridPxW() int { return m.Cols * m.CellW }
func (m TermMetrics) GridPxH() int { return m.Rows * m.CellH }

type rawTermState struct {
	fd   int
	orig syscall.Termios
}

func enableRawMode(fd int) (*rawTermState, error) {
	var orig syscall.Termios
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(ioctlGetTermios),
		uintptr(unsafe.Pointer(&orig)),
	)
	if errno != 0 {
		return nil, errno
	}

	raw := orig
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Cflag |= syscall.CS8
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	_, _, errno = syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(ioctlSetTermios),
		uintptr(unsafe.Pointer(&raw)),
	)
	if errno != 0 {
		return nil, errno
	}
	return &rawTermState{fd: fd, orig: orig}, nil
}

func (s *rawTermState) Restore() {
	if s == nil {
		return
	}
	_, _, _ = syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(s.fd),
		uintptr(ioctlSetTermios),
		uintptr(unsafe.Pointer(&s.orig)),
	)
}

// QueryMetrics reads TIOCGWINSZ and derives cell pixel dimensions, applying optional overrides from CSI 16t/14t.
func QueryMetrics(ttyFD int, csiCellW, csiCellH int) TermMetrics {
	cols, rows, xpx, ypx, err := getWinsize(ttyFD)
	if err != nil || cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}

	cellW, cellH := 16, 36
	hasPx := false

	if csiCellW > 0 && csiCellH > 0 {
		cellW, cellH = csiCellW, csiCellH
		hasPx = true
	} else if xpx > 0 && ypx > 0 {
		cw := xpx / cols
		ch := ypx / rows
		if cw >= 2 && ch >= 4 {
			cellW, cellH = cw, ch
			hasPx = true
		}
	}

	// If logical 1x points were reported (e.g. 8x18 CSS/point metrics on a 2x Retina display),
	// upsample sub-cell coordinates 2x while preserving the exact cell aspect ratio so textures
	// stay Retina-sharp and motion has >=16 sub-steps per cell.
	if cellW > 0 && cellW < 12 {
		scale := (15 + cellW) / cellW // 2x for 8..11px
		cellW *= scale
		cellH *= scale
	}

	return TermMetrics{
		Cols:  cols,
		Rows:  rows,
		CellW: cellW,
		CellH: cellH,
		HasPx: hasPx,
	}
}

// ProbeCapabilities sends a Kitty Graphics query + XTWINOPS cell/window pixel queries (CSI 16t + 14t)
// followed by a Primary Device Attributes query (CSI c) as an in-order FIFO sentinel.
// Because every VT100-compatible terminal answers CSI c immediately, non-Kitty terminals
// finish probing in ~1ms without waiting for a timeout.
func ProbeCapabilities(in *os.File, out *os.File) (kittySupported bool, cellW, cellH int) {
	envTerm := strings.ToLower(os.Getenv("TERM"))
	envProg := strings.ToLower(os.Getenv("TERM_PROGRAM"))
	envHintKitty := strings.Contains(envTerm, "kitty") ||
		strings.Contains(envTerm, "ghostty") ||
		strings.Contains(envProg, "ghostty") ||
		os.Getenv("GHOSTTY_RESOURCES_DIR") != "" ||
		os.Getenv("KITTY_WINDOW_ID") != ""

	// Query image id=31 (1x1 RGB) + CSI 16t (cell px) + CSI 14t (window px) + CSI c (DA1 sentinel).
	const query = "\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\\x1b[16t\x1b[14t\x1b[c"
	_, _ = out.WriteString(query)

	fd := int(in.Fd())
	_ = syscall.SetNonblock(fd, true)
	defer func() { _ = syscall.SetNonblock(fd, false) }()

	deadline := time.Now().Add(120 * time.Millisecond)
	var resp bytes.Buffer
	var tmp [256]byte

	for time.Now().Before(deadline) {
		n, err := syscall.Read(fd, tmp[:])
		if n > 0 {
			resp.Write(tmp[:n])
			// Once the DA1 response (\x1b[?...c) has arrived, all preceding FIFO responses are already in resp.
			if hasDA1Response(resp.Bytes()) {
				break
			}
		} else if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			time.Sleep(2 * time.Millisecond)
		} else if err != nil {
			break
		}
	}

	raw := resp.String()
	if strings.Contains(raw, "\x1b_Gi=31;OK") {
		kittySupported = true
	} else if envHintKitty && len(raw) == 0 {
		kittySupported = true
	}

	cellW, cellH = parseCSI16t(raw)
	if cellW == 0 || cellH == 0 {
		if winW, winH := parseCSI14t(raw); winW > 0 && winH > 0 {
			cols, rows, _, _, err := getWinsize(int(out.Fd()))
			if err == nil && cols > 0 && rows > 0 {
				cellW = winW / cols
				cellH = winH / rows
			}
		}
	}
	// Clean up query image id 31 in case a terminal stored it.
	if kittySupported {
		_, _ = out.WriteString("\x1b_Ga=d,d=I,i=31,q=2\x1b\\")
	}
	return kittySupported, cellW, cellH
}

func hasDA1Response(b []byte) bool {
	search := b
	for {
		idx := bytes.Index(search, []byte("\x1b[?"))
		if idx < 0 {
			return false
		}
		rest := search[idx+3:]
		valid := true
		for i := 0; i < len(rest); i++ {
			ch := rest[i]
			if ch == 'c' {
				return i > 0 && valid
			}
			if (ch < '0' || ch > '9') && ch != ';' {
				valid = false
				break
			}
		}
		if valid {
			return false
		}
		search = search[idx+3:]
	}
}

// parseCSI16t extracts (cellW, cellH) from "\x1b[6;<height>;<width>t".
func parseCSI16t(s string) (cellW, cellH int) {
	return parseCSITwoParams(s, "\x1b[6;")
}

// parseCSI14t extracts (winW, winH) from "\x1b[4;<height>;<width>t".
func parseCSI14t(s string) (winW, winH int) {
	return parseCSITwoParams(s, "\x1b[4;")
}

func parseCSITwoParams(s, prefix string) (w, h int) {
	rest := s
	for {
		idx := strings.Index(rest, prefix)
		if idx < 0 {
			return 0, 0
		}
		after := rest[idx+len(prefix):]
		end := strings.IndexByte(after, 't')
		if end < 0 {
			return 0, 0
		}
		body := after[:end]
		if pw, ph, ok := parseTwoDigitsSemi(body); ok {
			return pw, ph
		}
		rest = after
	}
}

func parseTwoDigitsSemi(body string) (w, h int, ok bool) {
	semi := strings.IndexByte(body, ';')
	if semi <= 0 || semi >= len(body)-1 {
		return 0, 0, false
	}
	a, b := body[:semi], body[semi+1:]
	if !isAllDigits(a) || !isAllDigits(b) {
		return 0, 0, false
	}
	ph, err1 := strconv.Atoi(a)
	pw, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || pw <= 0 || ph <= 0 || pw > 65535 || ph > 65535 {
		return 0, 0, false
	}
	return pw, ph, true
}

func isAllDigits(s string) bool {
	if len(s) == 0 || len(s) > 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

type InputAction int

const (
	ActionNone InputAction = iota
	ActionQuit
	ActionToggleStats
	ActionTogglePause
	ActionAimCorner
	ActionSpeedUp
	ActionSpeedDown
	ActionSizeUp
	ActionSizeDown
	ActionCycleMode
	ActionNextColor
	ActionRandomize
	ActionRedraw
	ActionAnyOtherKey
)

type InputEvent struct {
	Action InputAction
	CellW  int
	CellH  int
}

// InputParser reassembles raw terminal byte chunks across arbitrary Read() boundaries
// without misinterpreting split CSI/APC replies as user Esc or keypresses.
type InputParser struct {
	buf []byte
}

// NeedsTimeout reports whether the buffer holds an incomplete sequence (e.g. a lone 0x1b Esc byte)
// that should be flushed if no follow-up bytes arrive within a short Esc timeout.
func (p *InputParser) NeedsTimeout() bool {
	return len(p.buf) > 0
}

// FlushTimeout resolves a pending lone Esc or incomplete UTF-8 fragment after the disambiguation timeout expires.
func (p *InputParser) FlushTimeout(events chan<- InputEvent) {
	if len(p.buf) == 0 {
		return
	}
	if p.buf[0] == 0x1b {
		if len(p.buf) == 1 {
			p.buf = p.buf[:0]
			events <- InputEvent{Action: ActionQuit}
			return
		}
		// Incomplete stale escape sequence: discard cleanly without spurious keypresses.
		p.buf = p.buf[:0]
		return
	}
	p.buf = p.buf[:0]
	events <- InputEvent{Action: ActionAnyOtherKey}
}

// Feed appends raw bytes and emits all complete InputEvents.
func (p *InputParser) Feed(data []byte, events chan<- InputEvent) {
	if len(data) == 0 {
		return
	}
	if len(p.buf)+len(data) > 4096 {
		p.buf = p.buf[:0]
		if len(data) > 4096 {
			data = data[len(data)-4096:]
		}
	}
	p.buf = append(p.buf, data...)

	for len(p.buf) > 0 {
		b := p.buf[0]
		if b == 0x1b {
			if len(p.buf) == 1 {
				// Wait briefly to see whether trailing escape sequence bytes follow.
				return
			}
			next := p.buf[1]

			// APC sequence: \x1b_ ... \x1b\\ (or BEL \x07)
			if next == '_' {
				stIdx := bytes.Index(p.buf[2:], []byte("\x1b\\"))
				belIdx := bytes.IndexByte(p.buf[2:], 0x07)
				if stIdx >= 0 && (belIdx < 0 || stIdx < belIdx) {
					p.buf = p.buf[2+stIdx+2:]
					continue
				}
				if belIdx >= 0 {
					p.buf = p.buf[2+belIdx+1:]
					continue
				}
				if len(p.buf) > 1024 {
					p.buf = p.buf[2:]
					continue
				}
				return
			}

			// CSI sequence: \x1b[ <param/intermediate 0x20..0x3F>* <final 0x40..0x7E>
			if next == '[' {
				j := 2
				for j < len(p.buf) && p.buf[j] >= 0x20 && p.buf[j] <= 0x3F {
					j++
				}
				if j >= len(p.buf) {
					if len(p.buf) > 256 {
						p.buf = p.buf[2:]
						continue
					}
					return
				}
				final := p.buf[j]
				if final < 0x40 || final > 0x7E {
					// Interrupted/aborted CSI (e.g. another Esc byte): drop prefix up to j.
					p.buf = p.buf[j:]
					continue
				}
				seq := string(p.buf[:j+1])
				p.buf = p.buf[j+1:]
				if cw, ch := parseCSI16t(seq); cw > 0 && ch > 0 {
					events <- InputEvent{CellW: cw, CellH: ch}
					continue
				}
				switch seq {
				case "\x1b[A", "\x1b[C":
					events <- InputEvent{Action: ActionSpeedUp}
				case "\x1b[B", "\x1b[D":
					events <- InputEvent{Action: ActionSpeedDown}
				}
				// All other CSI replies (DA1, 14t, focus \x1b[I/\x1b[O, mouse SGR) are ignored cleanly.
				continue
			}

			// SS3 sequence (application-mode arrow keys: \x1bOA .. \x1bOD).
			if next == 'O' {
				if len(p.buf) < 3 {
					return
				}
				code := p.buf[2]
				p.buf = p.buf[3:]
				switch code {
				case 'A', 'C':
					events <- InputEvent{Action: ActionSpeedUp}
				case 'B', 'D':
					events <- InputEvent{Action: ActionSpeedDown}
				default:
					events <- InputEvent{Action: ActionAnyOtherKey}
				}
				continue
			}

			// Consecutive Esc byte: first Esc was standalone quit.
			if next == 0x1b {
				p.buf = p.buf[1:]
				events <- InputEvent{Action: ActionQuit}
				continue
			}

			// Alt+<key> two-byte sequence.
			p.buf = p.buf[2:]
			events <- InputEvent{Action: ActionAnyOtherKey}
			continue
		}

		// Multi-byte UTF-8 rune: wait if incomplete, then emit a single event per rune.
		if b >= 0x80 {
			if !utf8.FullRune(p.buf) && len(p.buf) < utf8.UTFMax {
				return
			}
			_, size := utf8.DecodeRune(p.buf)
			p.buf = p.buf[max(1, size):]
			events <- InputEvent{Action: ActionAnyOtherKey}
			continue
		}

		p.buf = p.buf[1:]
		switch b {
		case 3, 4, 'q', 'Q': // Ctrl+C, Ctrl+D, q
			events <- InputEvent{Action: ActionQuit}
		case 0x0c: // Ctrl+L
			events <- InputEvent{Action: ActionRedraw}
		case 's', 'S', 'i', 'I', '?':
			events <- InputEvent{Action: ActionToggleStats}
		case ' ':
			events <- InputEvent{Action: ActionTogglePause}
		case 'c', 'C':
			events <- InputEvent{Action: ActionAimCorner}
		case 'r', 'R':
			events <- InputEvent{Action: ActionRandomize}
		case '+', '=':
			events <- InputEvent{Action: ActionSpeedUp}
		case '-', '_':
			events <- InputEvent{Action: ActionSpeedDown}
		case ']':
			events <- InputEvent{Action: ActionSizeUp}
		case '[':
			events <- InputEvent{Action: ActionSizeDown}
		case 'm', 'M':
			events <- InputEvent{Action: ActionCycleMode}
		case 'n', 'N':
			events <- InputEvent{Action: ActionNextColor}
		default:
			events <- InputEvent{Action: ActionAnyOtherKey}
		}
	}
}

// StartInputReader reads raw terminal bytes and dispatches parsed InputEvents with a 25ms
// Esc disambiguation timer so split SSH/PTY escape sequences never misfire.
func StartInputReader(in *os.File, events chan<- InputEvent) {
	rawCh := make(chan []byte, 16)
	go func() {
		defer close(rawCh)
		var buf [256]byte
		for {
			n, err := in.Read(buf[:])
			if n > 0 {
				pkt := make([]byte, n)
				copy(pkt, buf[:n])
				rawCh <- pkt
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		var parser InputParser
		var escTimer *time.Timer
		var escCh <-chan time.Time
		const escDelay = 25 * time.Millisecond

		stopTimer := func() {
			if escTimer != nil {
				escTimer.Stop()
				escCh = nil
			}
		}

		for {
			select {
			case pkt, ok := <-rawCh:
				stopTimer()
				if !ok {
					parser.FlushTimeout(events)
					return
				}
				parser.Feed(pkt, events)
				if parser.NeedsTimeout() {
					if escTimer == nil {
						escTimer = time.NewTimer(escDelay)
					} else {
						escTimer.Reset(escDelay)
					}
					escCh = escTimer.C
				}
			case <-escCh:
				escCh = nil
				parser.FlushTimeout(events)
			}
		}
	}()
}
