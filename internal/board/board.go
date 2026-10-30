// Package board talks to an Arduino UNO Q's Linux side. Every transport runs bash scripts as the
// login user and moves files; everything above it is transport-agnostic.
package board

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Transport is a live connection to one board.
type Transport interface {
	// Run executes a bash script (fed on stdin to `bash -s`) as the login user.
	Run(ctx context.Context, script string, stdout, stderr io.Writer) error
	// Exec runs a single shell command line, with optional stdin.
	Exec(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error
	// Upload writes r to a file on the board.
	Upload(ctx context.Context, r io.Reader, remotePath string) error
	// Download copies a file from the board to w.
	Download(ctx context.Context, remotePath string, w io.Writer) error
	// Describe is a short human label such as "arduino@duck.local".
	Describe() string
	Close() error
}

// Kind of transport.
const (
	KindSSH = "ssh"
	KindADB = "adb"
)

// Target says how to reach a board. It is what gets saved in the config. A board can be reached
// over the network (Host) and over USB (Serial, or its serial number BoardID: a UNO Q's adb serial
// is its serial number); Via picks between the two.
type Target struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`              // ssh when Host is set, else adb
	Host    string `json:"host,omitempty"`    // ssh
	Port    int    `json:"port,omitempty"`    // ssh, default 22
	User    string `json:"user,omitempty"`    // ssh, default arduino
	KeyFile string `json:"keyFile,omitempty"` // ssh, optional private key
	Serial  string `json:"serial,omitempty"`  // adb, optional when only one device
	// BoardID is the board's serial number, learnt on connection: it identifies the board whatever
	// its address or name (Name is the board's own hostname).
	BoardID string `json:"boardId,omitempty"`
	// LastIP is the board's last known IPv4 address, tried when Host (usually NAME.local) does not
	// resolve.
	LastIP string `json:"lastIp,omitempty"`
	// SkipDesktop is a choice remembered for this board: its system updates leave out the on-board
	// desktop (App Lab, Chromium), which a board used from a computer does not need.
	SkipDesktop bool `json:"skipDesktop,omitempty"`
	// Via is the way to a board Quay can reach both ways: "" tries the network first, then USB when
	// the network does not answer and the board is plugged in; KindSSH or KindADB uses that way only.
	Via string `json:"via,omitempty"`
}

// USB is the target that finds the board on USB, and whether Quay knows how to: by the serial
// saved, else by the board's serial number. An "adb" target without a serial takes the only device.
func (t Target) USB() (Target, bool) {
	serial := t.Serial
	if serial == "" {
		serial = t.BoardID
	}
	if serial == "" && (t.Kind != KindADB || t.Host != "") {
		return Target{}, false
	}
	return Target{Name: t.Name, Kind: KindADB, Serial: serial, BoardID: t.BoardID}, true
}

// ParseTarget accepts "user@host[:port]", "ssh://user@host:port", "host", "adb" and "adb:SERIAL".
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Target{}, fmt.Errorf("empty board address")
	case s == "adb" || s == "usb":
		return Target{Kind: KindADB}, nil
	case strings.HasPrefix(s, "adb:"):
		return Target{Kind: KindADB, Serial: strings.TrimPrefix(s, "adb:")}, nil
	}
	s = strings.TrimPrefix(s, "ssh://")
	t := Target{Kind: KindSSH, User: "arduino", Port: 22}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		t.User, s = s[:at], s[at+1:]
	}
	if host, port, err := net.SplitHostPort(s); err == nil {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return Target{}, fmt.Errorf("bad port in %q", s)
		}
		t.Host, t.Port = host, p
	} else {
		t.Host = strings.Trim(s, "[]")
	}
	if t.Host == "" || t.User == "" {
		return Target{}, fmt.Errorf("cannot parse board address %q", s)
	}
	return t, nil
}

// CheckWays refuses a saved board Quay could not reach the way it is set to.
func (t Target) CheckWays() error {
	_, usb := t.USB()
	switch {
	case t.Via != "" && t.Via != KindSSH && t.Via != KindADB:
		return fmt.Errorf("unknown way to the board %q", t.Via)
	case t.Host == "" && !usb:
		return errors.New("enter the board's host name or IP address")
	case t.Via == KindSSH && t.Host == "":
		return errors.New("to use only the network, enter the board's host name or IP address")
	case t.Via == KindADB && !usb:
		return errors.New("Quay learns the board's USB serial number on the first connection over USB or the network")
	}
	return nil
}

// Ways describes how Quay reaches a saved board, e.g. "arduino@duck.local, then USB 1216269555".
func (t Target) Ways() string {
	usb, hasUSB := t.USB()
	u := "USB"
	if usb.Serial != "" {
		u += " " + usb.Serial
	}
	switch {
	case t.Host == "" || !hasUSB:
		return t.String()
	case t.Via == KindSSH:
		return t.network() + " (network only; also " + u + ")"
	case t.Via == KindADB:
		return u + " (USB only; also " + t.network() + ")"
	}
	return t.network() + ", then " + u
}

// String renders the target back in ParseTarget syntax: the network address, unless the board
// is reached over USB only.
func (t Target) String() string {
	if t.Host == "" || t.Via == KindADB {
		if u, _ := t.USB(); u.Serial != "" {
			return "adb:" + u.Serial
		}
		return "adb"
	}
	return t.network()
}

// network is the network address in ParseTarget syntax, user@host[:port].
func (t Target) network() string {
	host := t.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if t.Port != 0 && t.Port != 22 {
		return fmt.Sprintf("%s@%s:%d", t.User, host, t.Port)
	}
	return t.User + "@" + host
}

// Prompter asks the human for secrets and trust decisions. CLI and GUI implement it.
type Prompter interface {
	// Secret asks for a password; ok=false means the user cancelled.
	Secret(ctx context.Context, title, message string) (value string, ok bool, err error)
	// TrustHostKey shows an unknown SSH host key fingerprint and asks whether to trust it.
	TrustHostKey(ctx context.Context, host, keyType, fingerprint string) (bool, error)
}

// Open connects to the board. One Quay can reach both ways goes by Via; by default the network
// first, then USB when nothing answers there and the board is plugged in, then the network again.
func Open(ctx context.Context, t Target, p Prompter, knownHostsFile string) (Transport, error) {
	usb, hasUSB := t.USB()
	switch t.Via {
	case KindADB:
		if !hasUSB {
			return nil, fmt.Errorf("%s is set to USB, but Quay does not know its serial number yet", t.Name)
		}
		return OpenADB(ctx, usb)
	case KindSSH:
		if t.Host == "" {
			return nil, fmt.Errorf("%s is set to the network, but has no address", t.Name)
		}
		return DialSSH(ctx, t, p, knownHostsFile)
	case "":
	default:
		return nil, fmt.Errorf("unknown way to the board %q", t.Via)
	}
	switch {
	case t.Host == "" && hasUSB:
		return OpenADB(ctx, usb)
	case !hasUSB:
		return DialSSH(ctx, t, p, knownHostsFile)
	}
	s, err := dialSSH(ctx, t, p, knownHostsFile, 1)
	var u *UnreachableError
	if err == nil {
		return s, nil
	}
	if !errors.As(err, &u) || ctx.Err() != nil {
		return nil, err // a wrong password, say, is not a reason to switch
	}
	a, usbErr := OpenADB(ctx, usb)
	if usbErr == nil {
		return a, nil
	}
	select {
	case <-time.After(redialDelay):
	case <-ctx.Done():
		return nil, err
	}
	if s, err = dialSSH(ctx, t, p, knownHostsFile, 1); err == nil {
		return s, nil
	}
	var again *UnreachableError
	if !errors.As(err, &again) {
		return nil, err
	}
	u.Rounds = append(u.Rounds, again.Rounds...)
	return nil, fmt.Errorf("%w; over USB: %v", u, usbErr)
}

// Quote returns s as a single-quoted shell word.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%_-+=:,./", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Output runs a script and returns stdout; stderr is folded into the error.
func Output(ctx context.Context, tr Transport, script string) (string, error) {
	var out, errb strings.Builder
	if err := tr.Run(ctx, script, &out, &errb); err != nil {
		return out.String(), withStderr(err, errb.String())
	}
	return out.String(), nil
}

func withStderr(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	if len(stderr) > 4000 {
		stderr = "…" + stderr[len(stderr)-4000:]
	}
	return fmt.Errorf("%w: %s", err, stderr)
}
