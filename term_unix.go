//go:build unix

package main

import (
	"bytes"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unsafe"
)

type rawTermState struct {
	fd   int
	orig syscall.Termios
}

func enableRawMode(inFD, _ int) (*rawTermState, error) {
	var orig syscall.Termios
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(inFD),
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
		uintptr(inFD),
		uintptr(ioctlSetTermios),
		uintptr(unsafe.Pointer(&raw)),
	)
	if errno != 0 {
		return nil, errno
	}
	return &rawTermState{fd: inFD, orig: orig}, nil
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

func getWinsize(fd int) (cols, rows, xpixel, ypixel int, err error) {
	var ws struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return 0, 0, 0, 0, errno
	}
	return int(ws.Col), int(ws.Row), int(ws.Xpixel), int(ws.Ypixel), nil
}

func openControllingTTY() (in *os.File, out *os.File, closeFn func(), err error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	return f, f, func() { _ = f.Close() }, nil
}

func isConsoleInput(fd int) bool {
	_, _, _, _, err := getWinsize(fd)
	return err == nil
}

func readProbeReply(in *os.File, timeout time.Duration, stopFn func([]byte) bool) string {
	fd := int(in.Fd())
	_ = syscall.SetNonblock(fd, true)
	defer func() { _ = syscall.SetNonblock(fd, false) }()

	deadline := time.Now().Add(timeout)
	var resp bytes.Buffer
	var tmp [256]byte
	backoff := 200 * time.Microsecond

	for time.Now().Before(deadline) {
		n, err := syscall.Read(fd, tmp[:])
		if n > 0 {
			resp.Write(tmp[:n])
			backoff = 200 * time.Microsecond
			if stopFn != nil && stopFn(resp.Bytes()) {
				break
			}
			continue
		}
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			time.Sleep(backoff)
			if backoff < time.Millisecond {
				backoff = min(time.Millisecond, backoff*2)
			}
			continue
		}
		if err != nil {
			break
		}
	}
	return resp.String()
}

func setupSignals() (sigExit, sigWinch, sigCont chan os.Signal, stop func()) {
	// Ignore SIGPIPE so closed PTY/SSH pipes surface as normal Write errors and run defer cleanup().
	signal.Ignore(syscall.SIGPIPE)

	sigExit = make(chan os.Signal, 2)
	signal.Notify(sigExit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)

	sigWinch = make(chan os.Signal, 2)
	signal.Notify(sigWinch, syscall.SIGWINCH)

	sigCont = make(chan os.Signal, 2)
	signal.Notify(sigCont, syscall.SIGCONT)

	stop = func() {
		signal.Stop(sigExit)
		signal.Stop(sigWinch)
		signal.Stop(sigCont)
	}
	return sigExit, sigWinch, sigCont, stop
}

func suspendProcess() bool {
	return syscall.Kill(syscall.Getpid(), syscall.SIGTSTP) == nil
}
