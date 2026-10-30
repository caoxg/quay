package board

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// PinHostKeys makes keys (authorized_keys format, as in /etc/ssh/ssh_host_*_key.pub) the host keys
// Quay trusts for host:port, replacing what its known_hosts file had for that name: a re-flashed
// board has new keys, and keys read over USB need no fingerprint check. ~/.ssh/known_hosts is left
// alone; a match in Quay's file is enough to connect.
func PinHostKeys(knownHostsFile, host string, port int, keys []string) error {
	if port == 0 {
		port = 22
	}
	name := knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))
	var lines []string
	for _, k := range keys {
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k))
		if err != nil {
			return fmt.Errorf("host key %q: %w", k, err)
		}
		lines = append(lines, knownhosts.Line([]string{name}, pub))
	}
	if len(lines) == 0 {
		return fmt.Errorf("no host keys for %s", host)
	}
	old, err := os.ReadFile(knownHostsFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var keep []string
	for _, l := range strings.Split(string(old), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if f := strings.Fields(l); len(f) > 0 && hasHost(f[0], name) {
			continue
		}
		keep = append(keep, l)
	}
	if err := os.MkdirAll(filepath.Dir(knownHostsFile), 0o700); err != nil {
		return err
	}
	tmp := knownHostsFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(append(keep, lines...), "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, knownHostsFile)
}

// hasHost tells whether a known_hosts host field (comma-separated names) lists name.
func hasHost(field, name string) bool {
	for _, h := range strings.Split(field, ",") {
		if h == name {
			return true
		}
	}
	return false
}
