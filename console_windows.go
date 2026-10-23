//go:build windows && !nogui

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

var attachConsoleProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")

// attachConsole lets the GUI build (linked as a Windows GUI program, so it has no console) print
// when it is started from a terminal with arguments. The terminal does not wait for a GUI program,
// so bin\quay.exe is the CLI to use; this only keeps `Quay.exe --version` and friends from
// printing nothing.
func attachConsole() {
	const attachParentProcess = ^uintptr(0) // (DWORD)-1
	if r, _, _ := attachConsoleProc.Call(attachParentProcess); r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout, os.Stderr = f, f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
		os.Stdin = f
	}
}
