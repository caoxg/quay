package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/caoxg/quay/internal/remote"
)

// Setup is what App Lab's first setup changes, as the board reports it.
type Setup struct {
	SSHActive  bool   `json:"sshActive"`
	SSHEnabled bool   `json:"sshEnabled"`
	Password   string `json:"password"` // passwd -S: P usable, L locked, NP none ("" unknown)
}

// Needed tells whether the board still needs its first setup: no SSH server, or no usable password.
// (A board reached over SSH has had it; the question is for boards on USB.)
func (s *Setup) Needed() bool {
	return s != nil && (!s.SSHActive || s.Password == "L" || s.Password == "NP")
}

// EnableSSH turns the board's SSH server on, as App Lab's first setup does, and returns the board's
// SSH host keys (authorized_keys format, one per line). They come over the connection in use — USB
// on a fresh board — so Quay can trust them without asking.
func (s *Session) EnableSSH(ctx context.Context, sink Sink) ([]string, error) {
	o := s.op(ctx, sink)
	var keys []string
	err := o.step("ssh", "Turn the SSH server on", func() error {
		out, err := o.quiet(remote.Script(remote.EnableSSH, nil))
		if err != nil {
			return err
		}
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, "ssh-") || strings.HasPrefix(l, "ecdsa-") {
				keys = append(keys, l)
			}
		}
		return nil
	})
	return keys, err
}

// WaitOnline waits up to secs seconds for the board to be on Wi-Fi with an address (after saving its
// first network, say), and returns the address.
func (s *Session) WaitOnline(ctx context.Context, sink Sink, secs int) (string, error) {
	o := s.op(ctx, sink)
	var ip string
	err := o.step("online", fmt.Sprintf("Wait for the board to join Wi-Fi (up to %d s)", secs), func() error {
		out, err := o.quiet(remote.Script(remote.WaitOnline, map[string]string{"SECS": fmt.Sprint(secs)}))
		ip = strings.TrimSpace(out)
		return err
	})
	return ip, err
}
