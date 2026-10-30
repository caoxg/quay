package board

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// ADB is a Transport over USB using the adb binary from Android platform-tools (on PATH, or QUAY_ADB).
type ADB struct {
	bin    string
	serial string
}

// ADBPath finds adb: QUAY_ADB, else the one on PATH, else (when that is missing or too old) the
// usual places of Android Studio's SDK and Homebrew.
func ADBPath() (string, error) {
	if p := os.Getenv("QUAY_ADB"); p != "" {
		return p, nil
	}
	var tooOld error
	if p, err := exec.LookPath("adb"); err == nil {
		if tooOld = checkADBVersion(p); tooOld == nil {
			return p, nil
		}
	}
	for _, p := range adbPlaces() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && checkADBVersion(p) == nil {
			return p, nil
		}
	}
	if tooOld != nil {
		return "", tooOld
	}
	return "", errors.New("adb not found: install Android platform-tools (macOS: brew install android-platform-tools) or use SSH")
}

// adbPlaces lists where adb usually is when it is not on PATH.
func adbPlaces() []string {
	exe := "adb"
	if runtime.GOOS == "windows" {
		exe = "adb.exe"
	}
	var ps []string
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if d := os.Getenv(env); d != "" {
			ps = append(ps, filepath.Join(d, "platform-tools", exe))
		}
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		ps = append(ps, filepath.Join(home, "Library", "Android", "sdk", "platform-tools", exe), "/opt/homebrew/bin/adb", "/usr/local/bin/adb")
	case "linux":
		ps = append(ps, filepath.Join(home, "Android", "Sdk", "platform-tools", exe), "/usr/lib/android-sdk/platform-tools/adb", "/usr/bin/adb")
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			ps = append(ps, filepath.Join(d, "Android", "Sdk", "platform-tools", exe))
		}
	}
	return ps
}

var adbVersionRe = regexp.MustCompile(`version 1\.0\.(\d+)`)

// checkADBVersion rejects adb releases from before 2018: they cannot talk to the board's adbd and,
// worse, kill the newer adb server other tools started.
func checkADBVersion(bin string) error {
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		return nil // let the real command report the problem
	}
	if m := adbVersionRe.FindStringSubmatch(string(out)); m != nil {
		if n, _ := strconv.Atoi(m[1]); n < 40 {
			return fmt.Errorf("%s is adb 1.0.%s, too old for the UNO Q: install current Android platform-tools (macOS: brew install --cask android-platform-tools) and make sure it comes first on PATH, or set QUAY_ADB", bin, m[1])
		}
	}
	return nil
}

// ADBDevice is one entry of `adb devices`.
type ADBDevice struct {
	Serial string `json:"serial"`
	State  string `json:"state"` // device | unauthorized | offline
}

// ADBDevices lists USB devices (network adb entries are ignored).
func ADBDevices(ctx context.Context) ([]ADBDevice, error) {
	bin, err := ADBPath()
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, bin, "devices").Output()
	if err != nil {
		return nil, fmt.Errorf("adb devices: %w", err)
	}
	var list []ADBDevice
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || f[0] == "List" || strings.HasPrefix(f[0], "*") || strings.Contains(f[0], ":") {
			continue
		}
		list = append(list, ADBDevice{Serial: f[0], State: f[1]})
	}
	return list, nil
}

// ADBInfo is what a USB device says about itself; all empty when it does not answer.
type ADBInfo struct {
	Hostname string
	Model    string
	SSH      string // systemctl is-active ssh: active, inactive…
	Password string // passwd -S on its own account: P usable, L locked, NP none
}

// ADBDescribe asks a USB device about itself, as its user (no root needed).
func ADBDescribe(ctx context.Context, serial string) ADBInfo {
	bin, err := ADBPath()
	if err != nil {
		return ADBInfo{}
	}
	out, err := exec.CommandContext(ctx, bin, "-s", serial, "shell",
		`hostname; tr -d '\0' </proc/device-tree/model 2>/dev/null; echo; systemctl is-active ssh 2>/dev/null; passwd -S 2>/dev/null`).Output()
	if err != nil {
		return ADBInfo{}
	}
	return parseADBInfo(string(out))
}

func parseADBInfo(out string) ADBInfo {
	l := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	for len(l) < 4 {
		l = append(l, "")
	}
	i := ADBInfo{Hostname: strings.TrimSpace(l[0]), Model: strings.TrimSpace(l[1]), SSH: strings.TrimSpace(l[2])}
	if f := strings.Fields(l[3]); len(f) > 1 {
		i.Password = f[1]
	}
	return i
}

// OpenADB picks the device (the only one, or t.Serial).
func OpenADB(ctx context.Context, t Target) (*ADB, error) {
	bin, err := ADBPath()
	if err != nil {
		return nil, err
	}
	devs, err := ADBDevices(ctx)
	if err != nil {
		return nil, err
	}
	var ready []string
	for _, d := range devs {
		if t.Serial != "" && d.Serial != t.Serial {
			continue
		}
		switch d.State {
		case "device":
			ready = append(ready, d.Serial)
		case "unauthorized":
			return nil, fmt.Errorf("board %s is connected but USB debugging is not authorized yet", d.Serial)
		}
	}
	switch {
	case len(ready) == 0 && t.Serial != "":
		return nil, fmt.Errorf("USB board %s not found", t.Serial)
	case len(ready) == 0:
		return nil, errors.New("no UNO Q found over USB (use a data cable)")
	case len(ready) > 1:
		return nil, fmt.Errorf("several USB devices (%s): choose one with adb:SERIAL", strings.Join(ready, ", "))
	}
	return &ADB{bin: bin, serial: ready[0]}, nil
}

func (a *ADB) Describe() string { return "usb:" + a.serial }

// Reverse makes connections to boardPort on the board's loopback arrive at localPort here.
func (a *ADB) Reverse(ctx context.Context, boardPort, localPort int) error {
	return a.run(a.cmd(ctx, "reverse", fmt.Sprintf("tcp:%d", boardPort), fmt.Sprintf("tcp:%d", localPort)), nil, nil, nil)
}

// RemoveReverse undoes Reverse.
func (a *ADB) RemoveReverse(ctx context.Context, boardPort int) error {
	return a.run(a.cmd(ctx, "reverse", "--remove", fmt.Sprintf("tcp:%d", boardPort)), nil, nil, nil)
}
func (a *ADB) Close() error { return nil }

func (a *ADB) cmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, a.bin, append([]string{"-s", a.serial}, args...)...)
}

func (a *ADB) run(c *exec.Cmd, stdin io.Reader, stdout, stderr io.Writer) error {
	var errb bytes.Buffer
	c.Stdin, c.Stdout = stdin, nonNil(stdout)
	c.Stderr = io.MultiWriter(nonNil(stderr), &errb)
	if err := c.Run(); err != nil {
		return withStderr(err, errb.String())
	}
	return nil
}

func (a *ADB) Run(ctx context.Context, script string, stdout, stderr io.Writer) error {
	return a.run(a.cmd(ctx, "shell", "-T", "bash -s"), strings.NewReader(script), stdout, stderr)
}

func (a *ADB) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	return a.run(a.cmd(ctx, "shell", "-T", cmd), stdin, stdout, stderr)
}

func (a *ADB) Upload(ctx context.Context, r io.Reader, remotePath string) error {
	f, err := os.CreateTemp("", "quay-upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	f.Close()
	var errb bytes.Buffer
	if err := a.run(a.cmd(ctx, "push", f.Name(), remotePath), nil, io.Discard, &errb); err != nil {
		return withStderr(fmt.Errorf("adb push: %w", err), errb.String())
	}
	return nil
}

func (a *ADB) Download(ctx context.Context, remotePath string, w io.Writer) error {
	var errb bytes.Buffer
	if err := a.run(a.cmd(ctx, "exec-out", "cat "+Quote(remotePath)), nil, w, &errb); err != nil {
		return withStderr(fmt.Errorf("adb exec-out: %w", err), errb.String())
	}
	return nil
}
