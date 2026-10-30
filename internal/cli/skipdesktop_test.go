package cli

import (
	"testing"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/config"
)

// --skip-desktop and --with-desktop are remembered on the saved board, found by its serial number
// even after a rename.
func TestRememberSkipDesktop(t *testing.T) {
	t.Setenv("QUAY_HOME", t.TempDir())
	cfg, _ := config.Load()
	cfg.Put(board.Target{Name: "duck", Kind: board.KindSSH, Host: "duck.local", BoardID: "1216269555"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	rememberSkipDesktop(board.Target{Name: "old-name", BoardID: "1216269555"}, true)
	cfg, _ = config.Load()
	if b, _ := cfg.Board("duck"); !b.SkipDesktop {
		t.Fatal("--skip-desktop was not remembered")
	}
	rememberSkipDesktop(board.Target{Name: "duck"}, false)
	cfg, _ = config.Load()
	if b, _ := cfg.Board("duck"); b.SkipDesktop {
		t.Fatal("--with-desktop did not bring the desktop back")
	}
	rememberSkipDesktop(board.Target{Name: "elsewhere", Kind: board.KindSSH, Host: "10.0.0.9"}, true) // not saved: nothing to remember
	cfg, _ = config.Load()
	if len(cfg.Boards) != 1 {
		t.Fatalf("boards: %+v", cfg.Boards)
	}
}
