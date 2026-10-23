//go:build !nogui

package main

import "github.com/caoxg/quay/internal/gui"

const guiAvailable = true

func runGUI() error { return gui.Run() }
