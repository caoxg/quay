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
	"regexp"
	"strconv"
	"strings"
)

// ADB is a Transport over USB using the adb binary from Android platform-tools (on PATH, or QUAY_ADB).
type ADB struct {
	bin    string
	serial string
}

// ADBPath finds adb.
func ADBPath() (string, error) {
	if p := os.Getenv("QUAY_ADB"); p != "" {
		return p, nil
	}
	p, err := exec.LookPath("adb")
	if err != nil {
		return "", errors.New("adb not found: install Android platform-tools (macOS: brew install android-platform-tools) or use SSH")
	}
	if err := checkADBVersion(p); err != nil {
		return "", err
	}
	return p, nil
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
