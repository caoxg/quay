//go:build windows

package cli

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// openTTY returns the console for prompts, even when stdin/stdout are redirected.
func openTTY() (in, out *os.File, err error) {
	in, err = os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	out, err = os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		in.Close()
		return nil, nil, err
	}
	return in, out, nil
}

// enableANSI turns on escape-sequence processing (colours, line clearing) in the console.
func enableANSI(f *os.File) bool {
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}

// watchResize polls the console size (Windows has no SIGWINCH) until stop is called.
func watchResize(fd int, fn func(w, h int)) (stop func()) {
	done := make(chan struct{})
	go func() {
		w0, h0, _ := term.GetSize(fd)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if w, h, err := term.GetSize(fd); err == nil && (w != w0 || h != h0) {
					w0, h0 = w, h
					fn(w, h)
				}
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}
