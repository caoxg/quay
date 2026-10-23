// Package relay lets a board download through the computer running Quay. Quay forwards a port on
// the board's loopback back over its own SSH (or adb) connection to the Proxy here, and a small apt
// hook on the board (remote.RelayOn) sends apt's downloads to that port while it is open. The
// computer may in turn use an upstream proxy (a local Clash, say), so a board without a good route
// to deb.debian.org or apt-repo.arduino.cc gets the computer's.
package relay

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Port is the port on the board's loopback that leads to the relay.
const Port = 18765

// Proxy is a small forward HTTP proxy: CONNECT for https, absolute-URI requests for plain http.
type Proxy struct {
	Upstream     *url.URL // optional HTTP proxy to chain through; nil connects directly
	AllowAnyPort bool     // by default only ports 80 and 443 can be reached
	once         sync.Once
	transport    *http.Transport
	dialer       net.Dialer
}

// Serve accepts connections on ln until it is closed.
func (p *Proxy) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	return srv.Serve(ln)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || (r.URL.Scheme != "http" && r.URL.Scheme != "https") {
		http.Error(w, "this is Quay's download relay: it only forwards proxy requests", http.StatusBadRequest)
		return
	}
	if !p.allowed(r.URL.Host, r.URL.Scheme) {
		http.Error(w, "port not allowed through Quay's relay", http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	resp, err := p.roundTripper().RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	if !p.allowed(r.Host, "https") {
		http.Error(w, "port not allowed through Quay's relay", http.StatusForbidden)
		return
	}
	upstream, err := p.dial(r.Context(), r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if n := buf.Reader.Buffered(); n > 0 { // bytes the client sent right after CONNECT
		b, _ := buf.Reader.Peek(n)
		_, _ = upstream.Write(b)
	}
	pipe(client, upstream)
}

// pipe copies both ways until either side closes.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}

func (p *Proxy) allowed(hostport, scheme string) bool {
	if p.AllowAnyPort {
		return true
	}
	_, port, err := net.SplitHostPort(hostport)
	if err != nil {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	return port == "80" || port == "443"
}

// dial opens a TCP connection to hostport, through the upstream proxy when there is one.
func (p *Proxy) dial(ctx context.Context, hostport string) (net.Conn, error) {
	if p.Upstream == nil {
		return p.dialer.DialContext(ctx, "tcp", hostport)
	}
	addr := p.Upstream.Host
	if p.Upstream.Port() == "" {
		addr = net.JoinHostPort(p.Upstream.Hostname(), "80")
	}
	c, err := p.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy %s: %w", p.Upstream.Host, err)
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: hostport}, Host: hostport, Header: http.Header{}}
	if u := p.Upstream.User; u != nil {
		pw, _ := u.Password()
		req.SetBasicAuth(u.Username(), pw)
		req.Header.Set("Proxy-Authorization", req.Header.Get("Authorization"))
		req.Header.Del("Authorization")
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("upstream proxy %s: %w", p.Upstream.Host, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, fmt.Errorf("upstream proxy %s refused %s: %s", p.Upstream.Host, hostport, resp.Status)
	}
	_ = c.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: c, r: br}, nil
	}
	return c, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (p *Proxy) roundTripper() http.RoundTripper {
	p.once.Do(func() {
		p.transport = &http.Transport{
			Proxy:                 http.ProxyURL(p.Upstream),
			DialContext:           p.dialer.DialContext,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}
	})
	return p.transport
}

// Upstream picks the proxy the relay chains through: the explicit setting, else HTTPS_PROXY /
// HTTP_PROXY, else the operating system's proxy settings (macOS, Windows). nil means direct.
func Upstream(explicit string) (*url.URL, error) {
	for _, s := range []string{explicit, os.Getenv("HTTPS_PROXY"), os.Getenv("https_proxy"), os.Getenv("HTTP_PROXY"), os.Getenv("http_proxy"), systemProxy()} {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "://") {
			s = "http://" + s
		}
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("bad proxy address %q", s)
		}
		if u.Scheme != "http" {
			return nil, fmt.Errorf("the relay can chain through an HTTP proxy only, not %s", u.Scheme)
		}
		return u, nil
	}
	return nil, nil
}
