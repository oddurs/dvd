package main

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	// Quick env fast-path hint in case stdin isn't responding, while still probing for exact 16t/14t pixels.
	envTerm := strings.ToLower(os.Getenv("TERM"))
	envProg := strings.ToLower(os.Getenv("TERM_PROGRAM"))
	envHintKitty := strings.Contains(envTerm, "kitty") ||
		strings.Contains(envTerm, "ghostty") ||
		strings.Contains(envProg, "ghostty") ||
		os.Getenv("GHOSTTY_RESOURCES_DIR") != "" ||
		os.Getenv("KITTY_WINDOW_ID") != ""

	// Query image id=31 (1x1 RGB) + CSI 16t (cell px) + CSI 14t (window px) + CSI c (DA1 sentinel).
	query := "\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\\x1b[16t\x1b[14t\x1b[c"
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
	// Clean up query image id 31 just in case a terminal stored it.
	if kittySupported {
		_, _ = out.WriteString("\x1b_Ga=d,d=I,i=31,q=2\x1b\\")
	}
	return kittySupported, cellW, cellH
}

func hasDA1Response(b []byte) bool {
	idx := bytes.Index(b, []byte("\x1b[?"))
	if idx < 0 {
		return false
	}
	for i := idx + 3; i < len(b); i++ {
		if b[i] == 'c' {
			return true
		}
	}
	return false
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
	idx := strings.Index(s, prefix)
	if idx < 0 {
		return 0, 0
	}
	rest := s[idx+len(prefix):]
	end := strings.IndexByte(rest, 't')
	if end < 0 {
		return 0, 0
	}
	parts := strings.Split(rest[:end], ";")
	if len(parts) != 2 {
		return 0, 0
	}
	ph, err1 := strconv.Atoi(parts[0])
	pw, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || pw <= 0 || ph <= 0 {
		return 0, 0
	}
	return pw, ph
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
	ActionAnyOtherKey
)

type InputEvent struct {
	Action InputAction
	CellW  int
	CellH  int
}

// StartInputReader reads raw terminal bytes and emits parsed InputEvents without misinterpreting
// terminal CSI/APC responses as user Esc presses.
func StartInputReader(in *os.File, events chan<- InputEvent) {
	go func() {
		var buf [256]byte
		for {
			n, err := in.Read(buf[:])
			if n <= 0 || err != nil {
				return
			}
			parseInputBytes(buf[:n], events)
		}
	}()
}

func parseInputBytes(data []byte, events chan<- InputEvent) {
	i := 0
	n := len(data)
	for i < n {
		b := data[i]
		if b == 0x1b {
			// Standalone Esc key press (no trailing sequence bytes in this read packet).
			if i+1 >= n {
				events <- InputEvent{Action: ActionQuit}
				return
			}
			next := data[i+1]
			// APC sequence: \x1b_ ... \x1b\\
			if next == '_' {
				end := bytes.Index(data[i+2:], []byte("\x1b\\"))
				if end >= 0 {
					i += 2 + end + 2
					continue
				}
				return
			}
			// CSI sequence: \x1b[ ... <final byte 0x40..0x7E>
			if next == '[' {
				j := i + 2
				for j < n && (data[j] < 0x40 || data[j] > 0x7E) {
					j++
				}
				if j >= n {
					return
				}
				seq := string(data[i : j+1])
				i = j + 1
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
				continue
			}
			// Any other Alt/Esc sequence.
			i += 2
			continue
		}

		i++
		switch b {
		case 3, 4, 'q', 'Q': // Ctrl+C, Ctrl+D, q
			events <- InputEvent{Action: ActionQuit}
		case 's', 'S', 'i', 'I', '?':
			events <- InputEvent{Action: ActionToggleStats}
		case ' ':
			events <- InputEvent{Action: ActionTogglePause}
		case 'c', 'C':
			events <- InputEvent{Action: ActionAimCorner}
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
