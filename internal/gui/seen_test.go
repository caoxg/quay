package gui

import (
	"testing"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/discover"
)

func TestSameBoard(t *testing.T) {
	duck := discover.Board{Name: "duck", Host: "duck.local", IPs: []string{"192.168.2.163"}, Serial: "1216269555"}
	for _, c := range []struct {
		t    board.Target
		want bool
	}{
		{board.Target{Host: "duck.local", BoardID: "1216269555"}, true},
		{board.Target{Host: "old-name.local", BoardID: "1216269555"}, true}, // renamed since: the serial decides
		{board.Target{Host: "duck.local", BoardID: "999"}, false},           // another board took the name
		{board.Target{Host: "duck.local."}, true},
		{board.Target{Host: "192.168.2.163"}, true},
		{board.Target{Host: "rig.local"}, false},
	} {
		if got := sameBoard(c.t, duck); got != c.want {
			t.Errorf("sameBoard(%+v) = %v, want %v", c.t, got, c.want)
		}
	}
}
