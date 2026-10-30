//go:build !nogui

// Package gui is Quay's desktop app: a Wails v3 window on the system web view (no bundled
// Chromium) around plain HTML/CSS/JS in ./frontend.
package gui

import (
	"embed"
	"io/fs"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/caoxg/quay/internal/applog"
	"github.com/caoxg/quay/internal/version"
)

//go:embed frontend
var assets embed.FS

// icon is the window/taskbar icon on Linux (macOS uses the bundle icon, Windows the .exe resource).
//
//go:embed icon.png
var icon []byte

// Run opens the window and blocks until the app quits.
func Run() error {
	front, err := fs.Sub(assets, "frontend")
	if err != nil {
		return err
	}
	applog.Default.Printf("Quay %s started (%s/%s)", version.Version, runtime.GOOS, runtime.GOARCH)
	svc := newService()
	app := application.New(application.Options{
		Name:        "Quay",
		Description: "Deploy Arduino Apps from git to an Arduino UNO Q",
		Icon:        icon,
		Services:    []application.Service{application.NewService(svc)},
		Assets:      application.AssetOptions{Handler: application.BundledAssetFileServer(front)},
		Mac:         application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: true},
		// Matches build/linux/io.github.caoxg.quay.desktop so the window gets the launcher's icon.
		Linux: application.LinuxOptions{ApplicationID: "io.github.caoxg.quay"},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "io.github.caoxg.quay",
		},
	})
	svc.app = app
	svc.SetMenuLang("en") // the page sets its language as soon as it loads
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Quay",
		Width:            1220,
		Height:           820,
		MinWidth:         980,
		MinHeight:        660,
		BackgroundColour: application.NewRGB(246, 247, 249),
		URL:              "/",
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 52,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInsetUnified,
		},
		// Software rendering: the UI is light, and WebKitGTK's GPU path is the usual cause of blank
		// windows on some drivers (wails#2977). The zero value would mean "always".
		Linux: application.LinuxWindow{Icon: icon, WebviewGpuPolicy: application.WebviewGpuPolicyNever},
	})
	return app.Run()
}
