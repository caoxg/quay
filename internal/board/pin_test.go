package board

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func hostKey(t *testing.T) (ssh.PublicKey, string) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " root@duck"
}

func TestPinHostKeys(t *testing.T) {
	file := filepath.Join(t.TempDir(), "known_hosts")
	oldKey, _ := hostKey(t)
	other, _ := hostKey(t)
	os.WriteFile(file, []byte(knownhosts.Line([]string{"duck.local"}, oldKey)+"\n"+knownhosts.Line([]string{"goose.local"}, other)+"\n"), 0o600)

	newKey, line := hostKey(t)
	if err := PinHostKeys(file, "duck.local", 22, []string{line}); err != nil {
		t.Fatal(err)
	}
	cb, err := knownhosts.New(file)
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.IPv4(192, 168, 1, 2), Port: 22}
	if err := cb("duck.local:22", addr, newKey); err != nil {
		t.Fatalf("the new key is not trusted: %v", err)
	}
	if err := cb("duck.local:22", addr, oldKey); err == nil {
		t.Fatal("the re-flashed board's old key is still trusted")
	}
	if err := cb("goose.local:22", addr, other); err != nil {
		t.Fatalf("another board's key was dropped: %v", err)
	}
	if err := PinHostKeys(file, "duck.local", 22, []string{"not a key"}); err == nil {
		t.Fatal("accepted a broken key")
	}
}
