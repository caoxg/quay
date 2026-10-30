package board

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/caoxg/quay/internal/discover"
)

// SSH is a Transport over golang.org/x/crypto/ssh — no ssh/scp binaries needed.
type SSH struct {
	target   Target
	client   *ssh.Client
	password string        // remembered login password, offered to sudo later
	hostKey  ssh.PublicKey // the board's host key, as verified on connection
	mu       sync.Mutex

	lost    chan struct{} // closed when the connection is gone
	closing atomic.Bool   // Close was called: the connection was not lost
	silence time.Duration // set when the heartbeat gave up on a board that stopped answering
}

// LoginPassword returns the password used to log in, if any (to try with sudo).
func (s *SSH) LoginPassword() string { return s.password }

// DialSSH connects with ssh-agent keys, default key files, then password (asked via p).
func DialSSH(ctx context.Context, t Target, p Prompter, knownHostsFile string) (*SSH, error) {
	return dialSSH(ctx, t, p, knownHostsFile, 2)
}

// dialSSH is DialSSH with that many rounds of attempts to reach the board.
func dialSSH(ctx context.Context, t Target, p Prompter, knownHostsFile string, rounds int) (*SSH, error) {
	if t.Port == 0 {
		t.Port = 22
	}
	if t.User == "" {
		t.User = "arduino"
	}
	s := &SSH{target: t}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))

	signers := agentSigners()
	keyFiles := []string{t.KeyFile}
	if home, err := os.UserHomeDir(); err == nil {
		for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			keyFiles = append(keyFiles, filepath.Join(home, ".ssh", n))
		}
	}
	for _, f := range keyFiles {
		if f == "" {
			continue
		}
		if b, err := os.ReadFile(f); err == nil {
			if k, err := ssh.ParsePrivateKey(b); err == nil { // passphrase-protected keys come via the agent
				signers = append(signers, k)
			}
		}
	}

	askPassword := func() (string, error) {
		if s.password != "" {
			return s.password, nil
		}
		if p == nil {
			return "", errors.New("password required")
		}
		v, ok, err := p.Secret(ctx, "SSH login", fmt.Sprintf("Password for %s@%s", t.User, t.Host))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", context.Canceled
		}
		s.password = v
		return v, nil
	}
	auths := []ssh.AuthMethod{}
	if len(signers) > 0 {
		auths = append(auths, ssh.PublicKeys(signers...))
	}
	auths = append(auths,
		ssh.RetryableAuthMethod(ssh.PasswordCallback(askPassword), 1),
		ssh.RetryableAuthMethod(ssh.KeyboardInteractive(func(_, _ string, qs []string, echos []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range qs {
				pw, err := askPassword()
				if err != nil {
					return nil, err
				}
				ans[i] = pw
			}
			return ans, nil
		}), 1),
	)

	verify := hostKeyCallback(ctx, knownHostsFile, p)
	cfg := &ssh.ClientConfig{
		User: t.User,
		Auth: auths,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if err := verify(hostname, remote, key); err != nil {
				return err
			}
			s.hostKey = key
			return nil
		},
		HostKeyAlgorithms: knownAlgorithms(knownHostsFile, addr),
		Timeout:           12 * time.Second,
	}
	// The host key is always checked against the name in addr, even when dialling a fallback IP.
	nc, err := dialBoard(ctx, t, addr, rounds)
	if err != nil {
		return nil, err
	}
	conn := &heardConn{Conn: nc}
	conn.heard()
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		if strings.Contains(err.Error(), "unable to authenticate") {
			s.password = ""
			return nil, fmt.Errorf("login to %s@%s failed (wrong password or no accepted key)", t.User, t.Host)
		}
		return nil, fmt.Errorf("ssh %s: %w", addr, err)
	}
	s.client = ssh.NewClient(c, chans, reqs)
	s.watch(conn)
	return s, nil
}

// redialDelay is the pause before the second round of attempts; dialTimeout is how long one
// address may take to answer (a board on the local network answers in milliseconds).
var redialDelay, dialTimeout = 3 * time.Second, 6 * time.Second

// dialBoard opens the TCP connection to the board. When none of its addresses answers it tries
// them all again after redialDelay, up to rounds times in all: a board on Wi‑Fi now and then
// misses a first attempt and answers seconds later.
func dialBoard(ctx context.Context, t Target, addr string, rounds int) (net.Conn, error) {
	u := &UnreachableError{Addr: addr}
	for round := 0; round < rounds; round++ {
		if round > 0 {
			select {
			case <-time.After(redialDelay):
			case <-ctx.Done():
				return nil, u
			}
		}
		conn, failed := dialRound(ctx, t, addr)
		if conn != nil {
			return conn, nil
		}
		u.Rounds = append(u.Rounds, failed)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, u
}

// dialRound tries the board's address, then (for a name) its IPv4 addresses, and reports every
// address that failed.
func dialRound(ctx context.Context, t Target, addr string) (net.Conn, []DialFailure) {
	d := net.Dialer{Timeout: dialTimeout}
	first := dialAddress(ctx, t, addr)
	conn, err := d.DialContext(ctx, "tcp", first)
	if err == nil {
		return conn, nil
	}
	failed := []DialFailure{{Addr: first, Err: err}}
	if ctx.Err() != nil {
		return nil, failed
	}
	// Right after the computer wakes or joins a network, the system resolver can answer a .local
	// name with IPv6 addresses only (mDNS answers arrive one by one), and an IPv6 neighbour may
	// not be reachable yet ("no route to host"). The board's IPv4 address usually is.
	for _, alt := range ipv4Addresses(ctx, t) {
		if alt == first {
			continue
		}
		c, err := d.DialContext(ctx, "tcp", alt)
		if err == nil {
			return c, nil
		}
		failed = append(failed, DialFailure{Addr: alt, Err: err})
		if ctx.Err() != nil {
			break
		}
	}
	return nil, failed
}

// UnreachableError is a board that answered at none of its addresses: every address of every
// round of attempts, with why it failed.
type UnreachableError struct {
	Addr   string // as saved, e.g. duck.local:22
	Rounds [][]DialFailure
}

// DialFailure is one address that did not answer.
type DialFailure struct {
	Addr string
	Err  error
}

func (e *UnreachableError) Error() string {
	var rounds []string
	for _, r := range e.Rounds {
		var parts []string
		for _, f := range r {
			parts = append(parts, f.String())
		}
		rounds = append(rounds, strings.Join(parts, "; "))
	}
	switch {
	case len(rounds) == 0:
		return "cannot reach " + e.Addr
	case len(rounds) == 1:
		return fmt.Sprintf("cannot reach %s: %s", e.Addr, rounds[0])
	case rounds[0] == rounds[1]:
		return fmt.Sprintf("cannot reach %s (twice, %s apart): %s", e.Addr, redialDelay, rounds[0])
	}
	return fmt.Sprintf("cannot reach %s: %s; %s later: %s", e.Addr, rounds[0], redialDelay, rounds[1])
}

// Unwrap lets errors.Is see the dial errors, a cancelled context among them.
func (e *UnreachableError) Unwrap() []error {
	var errs []error
	for _, r := range e.Rounds {
		for _, f := range r {
			errs = append(errs, f.Err)
		}
	}
	return errs
}

// String gives the address and the cause; for a name, also the address it resolved to, which
// the system's dial error names.
func (f DialFailure) String() string {
	var op *net.OpError
	if !errors.As(f.Err, &op) {
		return f.Addr + ": " + f.Err.Error()
	}
	addr := f.Addr
	if op.Addr != nil && op.Addr.String() != f.Addr {
		addr += " (" + op.Addr.String() + ")"
	}
	return addr + ": " + op.Err.Error()
}

// dialAddress picks what to connect to. NAME.local names go through the system resolver first,
// then an mDNS query of our own (for resolvers without mDNS), then the last known IP.
func dialAddress(ctx context.Context, t Target, addr string) string {
	if net.ParseIP(t.Host) != nil {
		return addr
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(rctx, t.Host); err == nil {
		return addr
	}
	port := strconv.Itoa(t.Port)
	if strings.HasSuffix(strings.ToLower(strings.TrimSuffix(t.Host, ".")), ".local") {
		if ips, err := discover.LookupHost(ctx, t.Host, 2*time.Second); err == nil {
			return net.JoinHostPort(ips[0], port)
		}
	}
	if t.LastIP != "" {
		return net.JoinHostPort(t.LastIP, port)
	}
	return addr
}

// ipv4Addresses lists IPv4 addresses to try when connecting by name failed: our own mDNS query for
// NAME.local, then the last address the board was seen at.
func ipv4Addresses(ctx context.Context, t Target) []string {
	if net.ParseIP(t.Host) != nil {
		return nil
	}
	port := strconv.Itoa(t.Port)
	var out []string
	if strings.HasSuffix(strings.ToLower(strings.TrimSuffix(t.Host, ".")), ".local") {
		if ips, err := discover.LookupHost(ctx, t.Host, 2*time.Second); err == nil {
			for _, ip := range ips {
				out = append(out, net.JoinHostPort(ip, port))
			}
		}
	}
	if t.LastIP != "" {
		if a := net.JoinHostPort(t.LastIP, port); !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// TrustAs records the board's verified host key under another host name in Quay's known_hosts,
// so connecting by the board's new name after a rename does not ask to trust it again.
func (s *SSH) TrustAs(host string, port int, knownHostsFile string) error {
	if s.hostKey == nil || knownHostsFile == "" {
		return nil
	}
	if port == 0 {
		port = 22
	}
	if err := os.MkdirAll(filepath.Dir(knownHostsFile), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(knownHostsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))}, s.hostKey))
	return err
}

// The heartbeat: a board that loses power or leaves the Wi‑Fi sends nothing, not even a reset, and
// TCP alone takes minutes to give up on it. So the board must be heard from (any data, or the
// answer to a keepalive sent once it has been quiet for keepaliveAfter) at least every
// silenceLimit, or the connection is closed and Lost reports it.
var (
	heartbeatTick  = 2 * time.Second
	keepaliveAfter = 4 * time.Second
	silenceLimit   = 12 * time.Second
)

// watch starts the heartbeat on the connection c under s.client, and closes Lost when it ends.
func (s *SSH) watch(c *heardConn) {
	s.lost = make(chan struct{})
	go func() {
		s.client.Wait()
		close(s.lost)
	}()
	go s.heartbeat(c, heartbeatTick, keepaliveAfter, silenceLimit)
}

func (s *SSH) heartbeat(c *heardConn, tick, askAfter, limit time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	var asking atomic.Bool
	for {
		select {
		case <-s.lost:
			return
		case <-t.C:
		}
		switch quiet := c.quiet(); {
		case quiet >= limit:
			s.mu.Lock()
			s.silence = quiet
			s.mu.Unlock()
			s.client.Close()
			return
		case quiet >= askAfter && asking.CompareAndSwap(false, true):
			go func() {
				s.client.SendRequest("keepalive@openssh.com", true, nil) // any answer will do
				asking.Store(false)
			}()
		}
	}
}

// heardConn is the connection to the board, and when the board last sent something on it.
type heardConn struct {
	net.Conn
	last atomic.Int64 // time.Now().UnixNano() of the last read
}

func (c *heardConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.heard()
	}
	return n, err
}

func (c *heardConn) heard()               { c.last.Store(time.Now().UnixNano()) }
func (c *heardConn) quiet() time.Duration { return time.Since(time.Unix(0, c.last.Load())) }

// Lost is closed when the connection is gone: the board closed it (shutting down, say), stopped
// answering, or Close was called.
func (s *SSH) Lost() <-chan struct{} { return s.lost }

// LostReason says why the connection is gone, once Lost is closed; "" after Close.
func (s *SSH) LostReason() string {
	if s.closing.Load() {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.silence > 0 {
		return fmt.Sprintf("no answer from the board for %s", s.silence.Round(time.Second))
	}
	return "the board closed the connection"
}

// knownFiles are ~/.ssh/known_hosts and Quay's own file, when they exist.
func knownFiles(quayFile string) []string {
	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".ssh", "known_hosts"))
	}
	if quayFile != "" {
		files = append(files, quayFile)
	}
	var out []string
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			out = append(out, f)
		}
	}
	return out
}

// knownCallbacks parses each file on its own, so one unreadable line elsewhere does not disable
// host key checking for every board.
func knownCallbacks(quayFile string) []ssh.HostKeyCallback {
	var cbs []ssh.HostKeyCallback
	for _, f := range knownFiles(quayFile) {
		if cb, err := knownhosts.New(f); err == nil {
			cbs = append(cbs, cb)
		}
	}
	return cbs
}

// knownAlgorithms lists the host key algorithms already on file for addr, so the server is asked
// for a key type we can actually verify (otherwise a different key type looks like a changed key).
func knownAlgorithms(quayFile, addr string) []string {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	probe, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil
	}
	tcp, _ := net.ResolveTCPAddr("tcp", addr)
	seen := map[string]bool{}
	var algos []string
	for _, cb := range knownCallbacks(quayFile) {
		var ke *knownhosts.KeyError
		if err := cb(addr, tcp, probe.PublicKey()); errors.As(err, &ke) {
			for _, k := range ke.Want {
				t := k.Key.Type()
				names := []string{t}
				if t == ssh.KeyAlgoRSA {
					names = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
				}
				for _, n := range names {
					if !seen[n] {
						seen[n] = true
						algos = append(algos, n)
					}
				}
			}
		}
	}
	return algos
}

// hostKeyCallback checks the known_hosts files; unknown keys are shown to the user (trust on first
// use) and remembered in Quay's file. A changed key is always rejected.
func hostKeyCallback(ctx context.Context, quayFile string, p Prompter) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		changed := false
		for _, cb := range knownCallbacks(quayFile) {
			err := cb(hostname, remote, key)
			if err == nil {
				return nil
			}
			var ke *knownhosts.KeyError
			if errors.As(err, &ke) && len(ke.Want) > 0 {
				for _, w := range ke.Want {
					if w.Key.Type() == key.Type() {
						changed = true
					}
				}
			}
		}
		if changed {
			return fmt.Errorf("HOST KEY CHANGED for %s — refusing to connect. If the board was re-flashed, remove its old entry from %s", hostname, strings.Join(knownFiles(quayFile), " or "))
		}
		fp := ssh.FingerprintSHA256(key)
		if p == nil {
			return fmt.Errorf("unknown host key %s for %s", fp, hostname)
		}
		ok, err := p.TrustHostKey(ctx, hostname, key.Type(), fp)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("host key not trusted")
		}
		if quayFile != "" {
			_ = os.MkdirAll(filepath.Dir(quayFile), 0o700)
			line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
			if f, err := os.OpenFile(quayFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				fmt.Fprintln(f, line)
				f.Close()
			}
		}
		return nil
	}
}

func (s *SSH) Describe() string { return s.target.String() }

func (s *SSH) Close() error {
	if s.client == nil {
		return nil
	}
	s.closing.Store(true)
	return s.client.Close()
}

func (s *SSH) session(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	sess, err := s.client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, stderr
	if err := sess.Start(cmd); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("exit status %d", ee.ExitStatus())
		}
		return err
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		sess.Close()
		return ctx.Err()
	}
}

func (s *SSH) Run(ctx context.Context, script string, stdout, stderr io.Writer) error {
	return s.session(ctx, "bash -s", strings.NewReader(script), nonNil(stdout), nonNil(stderr))
}

func (s *SSH) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	return s.session(ctx, cmd, stdin, nonNil(stdout), nonNil(stderr))
}

func (s *SSH) Upload(ctx context.Context, r io.Reader, remotePath string) error {
	var errb bytes.Buffer
	if err := s.session(ctx, "cat > "+Quote(remotePath), r, io.Discard, &errb); err != nil {
		return withStderr(fmt.Errorf("upload %s: %w", remotePath, err), errb.String())
	}
	return nil
}

func (s *SSH) Download(ctx context.Context, remotePath string, w io.Writer) error {
	var errb bytes.Buffer
	if err := s.session(ctx, "cat "+Quote(remotePath), nil, w, &errb); err != nil {
		return withStderr(fmt.Errorf("download %s: %w", remotePath, err), errb.String())
	}
	return nil
}

// Client exposes the raw SSH client (interactive shell in the CLI).
func (s *SSH) Client() *ssh.Client { return s.client }

func nonNil(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// KeyBlob is the base64 part of an authorized_keys line ("" if there is none).
func KeyBlob(line string) string {
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, "AAAA") {
			return f
		}
	}
	return ""
}

// LocalPublicKey finds this computer's SSH public key: keyFile+".pub" when a key file is set for
// the board, else ~/.ssh/id_ed25519.pub, id_ecdsa.pub or id_rsa.pub. It returns the file and its
// authorized_keys line.
func LocalPublicKey(keyFile string) (path, line string, err error) {
	var candidates []string
	if keyFile != "" {
		candidates = append(candidates, strings.TrimSuffix(keyFile, ".pub")+".pub")
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			candidates = append(candidates, filepath.Join(home, ".ssh", n+".pub"))
		}
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if _, _, _, _, err := ssh.ParseAuthorizedKey(b); err == nil {
			return p, strings.TrimSpace(string(b)), nil
		}
	}
	return "", "", errors.New("no SSH public key found in ~/.ssh (id_ed25519.pub, id_ecdsa.pub or id_rsa.pub)")
}

// GenerateKey creates ~/.ssh/id_ed25519 and its .pub — what `ssh-keygen` does by default, without
// a passphrase (Quay reads key files directly; a passphrase needs ssh-agent) — for computers that
// have no SSH key yet. It never overwrites an existing key.
func GenerateKey() (path, line string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(home, ".ssh")
	priv := filepath.Join(dir, "id_ed25519")
	if _, err := os.Stat(priv); err == nil {
		return "", "", fmt.Errorf("%s already exists", priv)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	comment := "quay"
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME") // Windows
	}
	if h := hostnameOr(""); user != "" && h != "" {
		comment = user + "@" + h
	}
	block, err := ssh.MarshalPrivateKey(key, comment)
	if err != nil {
		return "", "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment
	// O_EXCL: never replace a key that appeared in the meantime.
	f, err := os.OpenFile(priv, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", err
	}
	if err := pem.Encode(f, block); err != nil {
		f.Close()
		return "", "", err
	}
	if err := f.Close(); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(priv+".pub", []byte(line+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return priv + ".pub", line, nil
}

func hostnameOr(def string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return strings.TrimSuffix(strings.Split(h, ".")[0], ".local")
	}
	return def
}
