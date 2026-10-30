package board

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTargetWays(t *testing.T) {
	both := Target{Name: "duck", Kind: KindSSH, Host: "duck.local", User: "arduino", Port: 22, BoardID: "1216269555"}
	if u, ok := both.USB(); !ok || u.Serial != "1216269555" || u.Kind != KindADB {
		t.Fatalf("USB of a board with a serial number: %+v %v", u, ok)
	}
	if _, ok := (Target{Kind: KindSSH, Host: "duck.local"}).USB(); ok {
		t.Fatal("a board never connected has no USB way")
	}
	if u, ok := (Target{Kind: KindADB}).USB(); !ok || u.Serial != "" {
		t.Fatal(`"adb" takes the only device`)
	}
	for via, want := range map[string]string{"": "arduino@duck.local, then USB 1216269555", KindSSH: "arduino@duck.local (network only; also USB 1216269555)", KindADB: "USB 1216269555 (USB only; also arduino@duck.local)"} {
		b := both
		b.Via = via
		if got := b.Ways(); got != want {
			t.Errorf("Ways via %q = %q, want %q", via, got, want)
		}
		if err := b.CheckWays(); err != nil {
			t.Errorf("CheckWays via %q: %v", via, err)
		}
	}
	if s := (Target{Host: "duck.local", User: "arduino", BoardID: "X", Via: KindADB}).String(); s != "adb:X" {
		t.Errorf("String of a USB-only board %q", s)
	}
	for _, bad := range []Target{{Name: "a"}, {Name: "b", Host: "b.local", Via: KindADB}, {Name: "c", BoardID: "C", Via: KindSSH}, {Name: "d", Host: "d.local", Via: "carrier pigeon"}} {
		if bad.CheckWays() == nil {
			t.Errorf("CheckWays accepted %+v", bad)
		}
	}
}

// fakeADB puts an adb on QUAY_ADB that lists the given "SERIAL STATE" lines.
func fakeADB(t *testing.T, devices ...string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "adb")
	script := "#!/bin/sh\necho 'List of devices attached'\n"
	for _, d := range devices {
		script += "echo '" + strings.ReplaceAll(d, " ", "\t") + "'\n"
	}
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUAY_ADB", p)
}

func TestOpenFallsBackToUSB(t *testing.T) {
	defer func(d time.Duration) { redialDelay = d }(redialDelay)
	redialDelay = 50 * time.Millisecond
	tg := Target{Name: "duck", Host: "127.0.0.1", Port: freePort(t), User: "arduino", BoardID: "SER1"}

	fakeADB(t, "SER1 device")
	tr, err := Open(context.Background(), tg, nil, "")
	if err != nil || tr.Describe() != "usb:SER1" {
		t.Fatalf("plugged in: %v %v", tr, err)
	}

	fakeADB(t, "OTHER device")
	_, err = Open(context.Background(), tg, nil, "")
	var u *UnreachableError
	if !errors.As(err, &u) || len(u.Rounds) != 2 || !strings.Contains(err.Error(), "; over USB: USB board SER1 not found") {
		t.Fatalf("not plugged in: %v", err)
	}

	tg.Via = KindSSH // never USB, however unreachable
	fakeADB(t, "SER1 device")
	if _, err = Open(context.Background(), tg, nil, ""); !errors.As(err, &u) || strings.Contains(err.Error(), "USB") {
		t.Fatalf("network only: %v", err)
	}
}
