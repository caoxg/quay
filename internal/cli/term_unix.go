//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// openTTY returns the controlling terminal for prompts, even when stdin/stdout are redirected.
func openTTY() (in, out *os.File, err error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	g, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, g, nil
}

func enableANSI(*os.File) bool { return true }

// watchResize forwards terminal size changes until stop is called.
func watchResize(fd int, fn func(w, h int)) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				if w, h, err := term.GetSize(fd); err == nil {
					fn(w, h)
				}
			case <-done:
				return
			}
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}
