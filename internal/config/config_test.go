package config

import (
	"testing"

	"github.com/caoxg/quay/internal/board"
)

func TestLearn(t *testing.T) {
	c := &Config{Default: "duck", Boards: []board.Target{
		{Name: "duck", Kind: board.KindSSH, Host: "duck.local", Port: 22, User: "arduino"},
		{Name: "bench", Kind: board.KindSSH, Host: "192.168.1.9", Port: 22, User: "arduino", BoardID: "B"},
		{Name: "usb", Kind: board.KindADB},
	}}

	// First connection: serial and address recorded, name already matches the hostname.
	if n, ch := c.Learn("duck", "duck", "A", "192.168.1.42"); n != "duck" || !ch {
		t.Fatalf("first learn: %q %v", n, ch)
	}
	if b, _ := c.Board("duck"); b.BoardID != "A" || b.LastIP != "192.168.1.42" {
		t.Fatalf("recorded: %+v", b)
	}
	if _, ch := c.Learn("duck", "duck", "A", "192.168.1.42"); ch {
		t.Fatal("nothing new should mean no change")
	}

	// Renamed on the board (by Quay or App Lab): the saved name, the default and the mDNS host follow.
	if n, ch := c.Learn("duck", "goose", "A", ""); n != "goose" || !ch {
		t.Fatalf("rename: %q %v", n, ch)
	}
	if b, ok := c.Board("goose"); !ok || b.Host != "goose.local" || c.Default != "goose" {
		t.Fatalf("after rename: %+v default=%s", b, c.Default)
	}

	// A different board with the same hostname gets a suffix; an IP address is never rewritten.
	if n, _ := c.Learn("bench", "goose", "B", "192.168.1.10"); n != "goose-2" {
		t.Fatalf("clash: %q", n)
	}
	if b, _ := c.Board("goose-2"); b.Host != "192.168.1.9" || b.LastIP != "" {
		t.Fatalf("IP-addressed board: %+v", b)
	}

	// USB boards are named after their hostname too, without an address.
	if n, _ := c.Learn("usb", "rig", "C", "10.0.0.5"); n != "rig" {
		t.Fatalf("usb: %q", n)
	}
	if b, _ := c.ByBoardID("C"); b.Name != "rig" || b.LastIP != "" {
		t.Fatalf("by id: %+v", b)
	}
	if n := c.UniqueName("goose", "A"); n != "goose" {
		t.Fatalf("a board keeps its own name: %q", n)
	}
	if n := c.UniqueName("goose", "Z"); n != "goose-3" {
		t.Fatalf("next free: %q", n)
	}
}

// A first setup renames the board while the board page keeps refreshing: whichever learns the new
// hostname first renames the saved board, and the other finds it by its serial number.
func TestLearnAfterAnotherRename(t *testing.T) {
	c := &Config{Default: "uno-q", Boards: []board.Target{{Name: "uno-q", Kind: board.KindADB, BoardID: "1216269555"}}}
	if n, ch := c.Learn("uno-q", "duck", "1216269555", ""); n != "duck" || !ch {
		t.Fatalf("first: %q %v", n, ch)
	}
	if n, ch := c.Learn("uno-q", "duck", "1216269555", ""); n != "duck" || ch {
		t.Fatalf("second, under the old name: %q %v", n, ch)
	}
	if n, ch := c.Learn("gone", "gone", "", ""); n != "gone" || ch {
		t.Fatalf("unknown board without a serial: %q %v", n, ch)
	}
}

func TestAdopt(t *testing.T) {
	c := &Config{Boards: []board.Target{
		{Name: "duck", Kind: board.KindSSH, Host: "duck.local", Port: 22, User: "arduino", BoardID: "A", LastIP: "192.168.2.163", SkipDesktop: true, Via: board.KindSSH},
		{Name: "rig", Kind: board.KindADB, Serial: "B", BoardID: "B"},
	}}

	// Over USB: the network address and the settings stay, and USB counts again.
	name, replaced, joined := c.Adopt(board.Target{Kind: board.KindADB, Serial: "A"}, "duck", "A")
	if b, _ := c.Board("duck"); name != "duck" || replaced != "" || !joined || b.Host != "duck.local" || b.LastIP != "192.168.2.163" || b.Serial != "A" || !b.SkipDesktop || b.Via != "" || b.Kind != board.KindSSH {
		t.Fatalf("USB onto a network board: %q %q %v %+v", name, replaced, joined, b)
	}

	// Over the network: a USB board gains its address.
	name, replaced, joined = c.Adopt(board.Target{Kind: board.KindSSH, Host: "rig.local", User: "arduino", Port: 22}, "rig", "B")
	if b, _ := c.Board("rig"); name != "rig" || replaced != "" || !joined || b.Host != "rig.local" || b.Serial != "B" || b.Kind != board.KindSSH {
		t.Fatalf("network onto a USB board: %q %q %v %+v", name, replaced, joined, b)
	}

	// Another address for the same board replaces the old one.
	if _, replaced, joined = c.Adopt(board.Target{Kind: board.KindSSH, Host: "192.168.2.50", User: "arduino", Port: 22}, "rig", "B"); replaced != "arduino@rig.local" || joined {
		t.Fatalf("new address: %q %v", replaced, joined)
	}

	// A board not saved yet is named after itself.
	if name, _, _ = c.Adopt(board.Target{Kind: board.KindADB, Serial: "C"}, "duck", "C"); name != "duck-2" {
		t.Fatalf("new board: %q", name)
	}
}
