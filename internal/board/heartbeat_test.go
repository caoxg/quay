package board

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fastHeartbeat makes the heartbeat give up after 400ms of silence, for the length of a test.
func fastHeartbeat(t *testing.T) {
	tick, after, limit := heartbeatTick, keepaliveAfter, silenceLimit
	heartbeatTick, keepaliveAfter, silenceLimit = 20*time.Millisecond, 100*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { heartbeatTick, keepaliveAfter, silenceLimit = tick, after, limit })
}

// sshServer is an SSH server on a local port that lets anyone in and answers keepalives; the
// connections it accepts come out of the returned channel.
func sshServer(t *testing.T) (string, <-chan *ssh.ServerConn) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	conns := make(chan *ssh.ServerConn, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				conns <- sc
				go ssh.DiscardRequests(reqs) // answers keepalive@openssh.com (with "no")
				for ch := range chans {
					ch.Reject(ssh.Prohibited, "no channels here")
				}
			}()
		}
	}()
	return l.Addr().String(), conns
}

// cable is a TCP relay to addr that can be cut like a board losing power: from then on it
// swallows everything and answers nothing, without closing the connection.
func cable(t *testing.T, addr string) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var cut atomic.Bool
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if err != nil {
				return
			}
			if !cut.Load() {
				dst.Write(buf[:n])
			}
		}
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			b, err := net.Dial("tcp", addr)
			if err != nil {
				c.Close()
				return
			}
			t.Cleanup(func() { c.Close(); b.Close() })
			go pipe(b, c)
			go pipe(c, b)
		}
	}()
	return l.Addr().String(), func() { cut.Store(true) }
}

// connect opens an SSH transport to addr the way DialSSH does after dialling.
func connect(t *testing.T, addr string) *SSH {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn := &heardConn{Conn: nc}
	conn.heard()
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{User: "arduino", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	s := &SSH{client: ssh.NewClient(c, chans, reqs)}
	s.watch(conn)
	t.Cleanup(func() { s.Close() })
	return s
}

func lostWithin(s *SSH, d time.Duration) bool {
	select {
	case <-s.Lost():
		return true
	case <-time.After(d):
		return false
	}
}

func TestHeartbeatNoticesASilentBoard(t *testing.T) {
	fastHeartbeat(t)
	srv, _ := sshServer(t)
	addr, cut := cable(t, srv)
	s := connect(t, addr)
	if lostWithin(s, 3*silenceLimit) {
		t.Fatalf("a quiet board that answers keepalives was given up: %s", s.LostReason())
	}
	cut()
	if !lostWithin(s, 3*silenceLimit) {
		t.Fatal("still connected to a board that answers nothing")
	}
	if r := s.LostReason(); !strings.HasPrefix(r, "no answer from the board for ") {
		t.Fatalf("reason %q", r)
	}
}

func TestLostWhenTheBoardHangsUpOrOnClose(t *testing.T) {
	fastHeartbeat(t)
	srv, conns := sshServer(t)
	s := connect(t, srv)
	(<-conns).Close() // the board shuts down
	if !lostWithin(s, time.Second) || s.LostReason() != "the board closed the connection" {
		t.Fatalf("after the board hung up: reason %q", s.LostReason())
	}

	s = connect(t, srv)
	s.Close()
	if !lostWithin(s, time.Second) || s.LostReason() != "" {
		t.Fatalf("after Close: reason %q", s.LostReason())
	}
}

func TestADBLostWhenUnplugged(t *testing.T) {
	defer func(d time.Duration) { usbCheckEvery = d }(usbCheckEvery)
	usbCheckEvery = 20 * time.Millisecond
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	bin := filepath.Join(dir, "adb") // `adb -s SER get-state` prints the state file, or fails without it
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec cat "+state+" 2>/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(state, []byte("device\n"), 0o644)
	a := &ADB{bin: bin, serial: "SER", closed: make(chan struct{})}
	select {
	case <-a.Lost():
		t.Fatal("lost while plugged in")
	case <-time.After(200 * time.Millisecond):
	}
	os.Remove(state)
	select {
	case <-a.Lost():
	case <-time.After(time.Second):
		t.Fatal("still there after the cable came out")
	}
	if a.LostReason() == "" {
		t.Fatal("no reason")
	}

	b := &ADB{bin: bin, serial: "SER", closed: make(chan struct{})}
	b.Lost()
	b.Close()
	select {
	case <-b.Lost():
	case <-time.After(time.Second):
		t.Fatal("Lost not closed after Close")
	}
	if b.LostReason() != "" {
		t.Fatalf("reason after Close %q", b.LostReason())
	}
}
