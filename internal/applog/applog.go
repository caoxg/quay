// Package applog keeps Quay's own log: what every operation did, one file a day, kept for two
// weeks, so a problem can be looked into after the activity panel is gone.
package applog

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Keep is how long a day's file stays.
const Keep = 14 * 24 * time.Hour

// Dir is where the files go: ~/Library/Logs/Quay on macOS, Quay\logs in the local app data folder
// on Windows, $XDG_STATE_HOME/quay/logs (~/.local/state/quay/logs) on Linux. QUAY_HOME, which
// moves Quay's settings, moves the log under it too.
func Dir() string {
	if d := os.Getenv("QUAY_HOME"); d != "" {
		return filepath.Join(d, "logs")
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Logs", "Quay")
	case "windows":
		if d, err := os.UserCacheDir(); err == nil { // %LocalAppData%
			return filepath.Join(d, "Quay", "logs")
		}
	default:
		if d := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(d) {
			return filepath.Join(d, "quay", "logs")
		}
		return filepath.Join(home, ".local", "state", "quay", "logs")
	}
	return filepath.Join(os.TempDir(), "quay-logs")
}

// Log appends lines to the day's file, quay-YYYY-MM-DD.log. It never fails its callers: a log
// that cannot be written must not break what it records.
type Log struct {
	dir string
	now func() time.Time

	mu  sync.Mutex
	day string
	f   *os.File
}

// Default is the log in Dir.
var Default = New(Dir())

// New is a log in dir.
func New(dir string) *Log { return &Log{dir: dir, now: time.Now} }

// Printf writes one entry, stamped with the local time. Lines after the first are indented, so an
// entry stays together.
func (l *Log) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	now := l.now()
	text := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\n    ")
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open(now) {
		fmt.Fprintf(l.f, "%s %s\n", now.Format("2006-01-02 15:04:05.000"), text)
	}
}

// Path is today's file (it exists once something was written today).
func (l *Log) Path() string { return filepath.Join(l.dir, "quay-"+l.now().Format("2006-01-02")+".log") }

// Dir is the folder of the files.
func (l *Log) Dir() string { return l.dir }

// open makes l.f the file of now's day, and drops the files older than Keep when it opens one.
func (l *Log) open(now time.Time) bool {
	day := now.Format("2006-01-02")
	if l.f != nil && l.day == day {
		return true
	}
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "quay-"+day+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false
	}
	l.f, l.day = f, day
	old, _ := filepath.Glob(filepath.Join(l.dir, "quay-*.log"))
	for _, p := range old {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "quay-"), ".log")
		if t, err := time.ParseInLocation("2006-01-02", name, now.Location()); err == nil && now.Sub(t) > Keep {
			_ = os.Remove(p)
		}
	}
	return true
}
