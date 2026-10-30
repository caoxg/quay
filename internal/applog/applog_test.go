package applog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogDaysAndRetention(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "quay-2026-10-01.log")
	recent := filepath.Join(dir, "quay-2026-10-20.log")
	other := filepath.Join(dir, "notes.txt")
	for _, p := range []string{stale, recent, other} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 28, 23, 59, 0, 0, time.Local)
	l := New(dir)
	l.now = func() time.Time { return now }
	l.Printf("upgrade: %s", "first line\nsecond line")
	now = now.Add(2 * time.Minute) // past midnight: a new file
	l.Printf("next day")

	b, err := os.ReadFile(filepath.Join(dir, "quay-2026-10-28.log"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-10-28 23:59:00.000 upgrade: first line\n    second line\n"; string(b) != want {
		t.Fatalf("day file %q, want %q", b, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "quay-2026-10-29.log")); !strings.HasSuffix(string(b), " next day\n") {
		t.Fatalf("next day's file %q", b)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a file older than two weeks was kept")
	}
	for _, p := range []string{recent, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was removed", filepath.Base(p))
		}
	}
	if got := l.Path(); got != filepath.Join(dir, "quay-2026-10-29.log") {
		t.Fatalf("Path %s", got)
	}
}

func TestNilLogIsQuiet(t *testing.T) {
	var l *Log
	l.Printf("nothing happens")
}
