// Quay deploys Arduino Apps from git to an Arduino® UNO Q.
// Arduino® is a trademark of Arduino S.r.l.; Quay is an independent project.
package main

import (
	"os"

	"github.com/caoxg/quay/internal/cli"
	"github.com/caoxg/quay/internal/selfupdate"
)

func main() {
	selfupdate.HasGUI = guiAvailable
	selfupdate.WaitForPrevious() // started by a self-update: let the old program exit first
	selfupdate.Cleanup()
	// No arguments: open the app (when built with the GUI). Otherwise behave as a CLI.
	if len(os.Args) == 1 && guiAvailable {
		if err := runGUI(); err != nil {
			os.Stderr.WriteString("quay: " + err.Error() + "\n")
			os.Exit(1)
		}
		return
	}
	attachConsole()
	var gui func() error
	if guiAvailable {
		gui = runGUI
	}
	os.Exit(cli.Execute(gui))
}
