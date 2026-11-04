//go:build windows

package main

import (
	"bytes"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procGetNumberOfConsoleInputEvs = kernel32.NewProc("GetNumberOfConsoleInputEvents")
	procReadConsoleInputW          = kernel32.NewProc("ReadConsoleInputW")
)

const (
	enableProcessedInput        = 0x0001
	enableLineInput             = 0x0002
	enableEchoInput             = 0x0004
	enableWindowInput           = 0x0008
	enableMouseInput            = 0x0010
	enableExtendedFlags         = 0x0080
	enableVirtualTerminalInput  = 0x0200
	enableProcessedOutput       = 0x0001
	enableVirtualTerminalOutput = 0x0004
	disableNewlineAutoReturn    = 0x0008
	keyEventRecordType          = 0x0001
)

type rawTermState struct {
	inH     syscall.Handle
	outH    syscall.Handle
	origIn  uint32
	origOut uint32
}

func enableRawMode(inFD, outFD int) (*rawTermState, error) {
	inH := syscall.Handle(inFD)
	outH := syscall.Handle(outFD)

	var origIn, origOut uint32
	r1, _, e1 := procGetConsoleMode.Call(uintptr(inH), uintptr(unsafe.Pointer(&origIn)))
	if r1 == 0 {
		return nil, e1
	}
	r2, _, e2 := procGetConsoleMode.Call(uintptr(outH), uintptr(unsafe.Pointer(&origOut)))
	if r2 == 0 {
		return nil, e2
	}

	rawIn := (origIn &^ (enableProcessedInput | enableLineInput | enableEchoInput)) |
		enableVirtualTerminalInput | enableWindowInput | enableMouseInput | enableExtendedFlags
	rawOut := origOut | enableProcessedOutput | enableVirtualTerminalOutput | disableNewlineAutoReturn

	if r, _, err := procSetConsoleMode.Call(uintptr(inH), uintptr(rawIn)); r == 0 {
		return nil, err
	}
	if r, _, err := procSetConsoleMode.Call(uintptr(outH), uintptr(rawOut)); r == 0 {
		_, _, _ = procSetConsoleMode.Call(uintptr(inH), uintptr(origIn))
		return nil, err
	}

	return &rawTermState{inH: inH, outH: outH, origIn: origIn, origOut: origOut}, nil
}

func (s *rawTermState) Restore() {
	if s == nil {
		return
	}
	_, _, _ = procSetConsoleMode.Call(uintptr(s.inH), uintptr(s.origIn))
	_, _, _ = procSetConsoleMode.Call(uintptr(s.outH), uintptr(s.origOut))
}

type consoleScreenBufferInfo struct {
	Size       struct{ X, Y int16 }
	CursorPos  struct{ X, Y int16 }
	Attributes uint16
	Window     struct{ Left, Top, Right, Bottom int16 }
	MaxWinSize struct{ X, Y int16 }
}

func getWinsize(fd int) (cols, rows, xpixel, ypixel int, err error) {
	outH := syscall.Handle(fd)
	if fd == 0 {
		h, gerr := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
		if gerr != nil {
			return 0, 0, 0, 0, gerr
		}
		outH = h
	}
	var info consoleScreenBufferInfo
	r, _, e := procGetConsoleScreenBufferInfo.Call(uintptr(outH), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0, 0, 0, 0, e
	}
	c := int(info.Window.Right-info.Window.Left) + 1
	rw := int(info.Window.Bottom-info.Window.Top) + 1
	return c, rw, 0, 0, nil
}

func isConsoleInput(fd int) bool {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(syscall.Handle(fd)), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}

func openControllingTTY() (in *os.File, out *os.File, closeFn func(), err error) {
	inF, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	outF, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		_ = inF.Close()
		return nil, nil, nil, err
	}
	return inF, outF, func() {
		_ = inF.Close()
		_ = outF.Close()
	}, nil
}

// inputRecord matches Windows INPUT_RECORD (20 bytes) so non-key console events
// (focus/resize/menu) never block VT capability probing.
type inputRecord struct {
	EventType       uint16
	_               uint16
	KeyDown         int32
	RepeatCount     uint16
	VirtualKeyCode  uint16
	VirtualScanCode uint16
	UnicodeChar     uint16
	ControlKeyState uint32
}

func readProbeReply(in *os.File, timeout time.Duration, stopFn func([]byte) bool) string {
	inH := syscall.Handle(in.Fd())
	deadline := time.Now().Add(timeout)
	var resp bytes.Buffer
	var recs [32]inputRecord
	var utfBuf [utf8.UTFMax]byte

	for time.Now().Before(deadline) {
		var count uint32
		r, _, _ := procGetNumberOfConsoleInputEvs.Call(uintptr(inH), uintptr(unsafe.Pointer(&count)))
		if r == 0 || count == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		toRead := min(uint32(len(recs)), count)
		var readN uint32
		r, _, _ = procReadConsoleInputW.Call(
			uintptr(inH),
			uintptr(unsafe.Pointer(&recs[0])),
			uintptr(toRead),
			uintptr(unsafe.Pointer(&readN)),
		)
		if r == 0 || readN == 0 {
			break
		}
		for i := uint32(0); i < readN; i++ {
			ev := recs[i]
			if ev.EventType == keyEventRecordType && ev.KeyDown != 0 && ev.UnicodeChar != 0 {
				n := utf8.EncodeRune(utfBuf[:], rune(ev.UnicodeChar))
				resp.Write(utfBuf[:n])
			}
		}
		if stopFn != nil && stopFn(resp.Bytes()) {
			break
		}
	}
	return resp.String()
}

func setupSignals() (sigExit, sigWinch, sigCont chan os.Signal, stop func()) {
	sigExit = make(chan os.Signal, 2)
	signal.Notify(sigExit, os.Interrupt)

	sigWinch = make(chan os.Signal, 2)
	sigCont = make(chan os.Signal, 1)

	done := make(chan struct{})
	go func() {
		prevC, prevR, _, _, _ := getWinsize(0)
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				c, r, _, _, err := getWinsize(0)
				if err == nil && (c != prevC || r != prevR) {
					prevC, prevR = c, r
					select {
					case sigWinch <- os.Interrupt:
					default:
					}
				}
			}
		}
	}()

	stop = func() {
		signal.Stop(sigExit)
		close(done)
	}
	return sigExit, sigWinch, sigCont, stop
}

func suspendProcess() bool {
	return false
}
