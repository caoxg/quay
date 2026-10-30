package selfupdate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Relaunch starts the updated program as soon as this process has exited; the caller then quits.
// (Started any earlier, the app's single-instance lock would hand it to the old process.)
func Relaunch(t Target) error {
	pid := strconv.Itoa(os.Getpid())
	var cmd *exec.Cmd
	switch t.Kind {
	case KindMacApp:
		cmd = exec.Command("/bin/sh", "-c", `while kill -0 "$1" 2>/dev/null; do sleep 0.2; done; exec open "$2"`, "sh", pid, t.Path)
	case KindWinZip:
		cmd = exec.Command(filepath.Join(t.Path, "Quay.exe"))
	default:
		cmd = exec.Command(t.Path)
	}
	cmd.Env = append(os.Environ(), "QUAY_WAIT_PID="+pid)
	detach(cmd)
	return cmd.Start()
}

// WaitForPrevious lets a program started by Relaunch wait (up to 20 s) for the one it replaces to
// exit. Call it first thing.
func WaitForPrevious() {
	pid, err := strconv.Atoi(os.Getenv("QUAY_WAIT_PID"))
	os.Unsetenv("QUAY_WAIT_PID")
	if err != nil || pid <= 0 {
		return
	}
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end) && running(pid); {
		time.Sleep(200 * time.Millisecond)
	}
}

// Check is the last update check, kept in a file so the CLI can mention an update without asking
// GitHub every time.
type Check struct {
	Checked time.Time `json:"checked"`
	Current string    `json:"current"`
	Latest  *Release  `json:"latest,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// Available reports whether the check found a newer release than the running one.
func (c *Check) Available() bool {
	return c != nil && c.Latest != nil && Newer(c.Latest.Version, versionNow())
}

// LoadCheck reads the last check from file ("" or missing: nil).
func LoadCheck(file string) *Check {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var c Check
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return &c
}

// Save writes the check to file.
func (c *Check) Save(file string) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, b, 0o644)
}
