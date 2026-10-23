package core

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/remote"
)

// A board's name is its hostname: one DNS label, which is also its mDNS name (NAME.local).
var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidHostname checks a new board name.
func ValidHostname(name string) error {
	if !hostnameRe.MatchString(name) {
		return fmt.Errorf("%q is not a valid board name: use lower-case letters, digits and hyphens (up to 63, not starting or ending with a hyphen)", name)
	}
	return nil
}

// Rename changes the board's name — its hostname, which is also its network name (NAME.local) and
// the name App Lab shows — the way App Lab does, with `arduino-app-cli system set-name`. The board
// image allows that without a password; older images fall back to hostnamectl through sudo.
func (s *Session) Rename(ctx context.Context, sink Sink, name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := ValidHostname(name); err != nil {
		return err
	}
	o := s.op(ctx, sink)
	cur, _ := o.quiet("hostname")
	if strings.TrimSpace(cur) == name {
		o.info("The board is already called "+name, nil)
		return nil
	}
	if err := o.step("rename", "Rename the board to "+name, func() error {
		if _, err := o.run(remote.Script(remote.SetName, map[string]string{"NAME": name}), nil); err == nil {
			return nil
		}
		o.warn("arduino-app-cli could not set the name; using hostnamectl instead")
		return o.sudo("Rename the board", `hostnamectl set-hostname "$NAME"`, map[string]string{"NAME": name})
	}); err != nil {
		return err
	}
	// avahi-daemon reads the hostname when it starts; restarting it announces NAME.local and the
	// renamed _arduino._tcp service right away (the image allows this without a password).
	_ = o.step("announce", "Announce "+name+".local on the network", func() error {
		_, err := o.quiet(remote.Script(remote.RestartAvahi, nil))
		if err != nil {
			o.warn("Could not restart avahi-daemon; the new name appears on the network after the next reboot")
			return errSkip
		}
		return nil
	})
	return o.step("verify", "Check the new name", func() error {
		got, err := o.quiet("hostname")
		if err != nil {
			return err
		}
		if strings.TrimSpace(got) != name {
			return fmt.Errorf("the board still reports the name %q", strings.TrimSpace(got))
		}
		return nil
	})
}

// TrustAs carries the board's host key over to a new host name (after a rename), for SSH sessions.
func (s *Session) TrustAs(host string, knownHostsFile string) {
	if t, ok := s.T.(*board.SSH); ok {
		_ = t.TrustAs(host, s.Target.Port, knownHostsFile)
	}
}
