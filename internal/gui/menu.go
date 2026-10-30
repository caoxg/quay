//go:build !nogui

package gui

import (
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// menuText is the macOS application menu's own items, in the app's language.
var menuText = map[string]map[string]string{
	"en": {"about": "About Quay", "settings": "Settings…", "update": "Check for Updates…"},
	"zh": {"about": "关于 Quay", "settings": "设置…", "update": "检查更新…"},
}

// SetMenuLang builds the macOS menu bar in the app's language (en or zh): the Quay menu with About,
// Settings (⌘,) and Check for Updates, which the page opens (event quay:menu), then Edit and Window.
func (s *Service) SetMenuLang(lang string) {
	if runtime.GOOS != "darwin" || s.app == nil {
		return
	}
	txt, ok := menuText[lang]
	if !ok {
		txt = menuText["en"]
	}
	open := func(what string) func(*application.Context) {
		return func(*application.Context) { s.emit("quay:menu", what) }
	}
	menu := application.NewMenu()
	quay := menu.AddSubmenu("Quay")
	quay.Add(txt["about"]).OnClick(open("about"))
	quay.AddSeparator()
	quay.Add(txt["settings"]).SetAccelerator("CmdOrCtrl+,").OnClick(open("settings"))
	quay.Add(txt["update"]).OnClick(open("update"))
	quay.AddSeparator()
	quay.AddRole(application.ServicesMenu)
	quay.AddSeparator()
	quay.AddRole(application.Hide)
	quay.AddRole(application.HideOthers)
	quay.AddRole(application.UnHide)
	quay.AddSeparator()
	quay.AddRole(application.Quit)
	menu.AddRole(application.EditMenu)
	menu.AddRole(application.WindowMenu)
	s.app.Menu.Set(menu)
}
