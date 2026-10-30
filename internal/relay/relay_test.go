package relay

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// serve runs p on a local port and returns its URL.
func serve(t *testing.T, p *Proxy) *url.URL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = p.Serve(ln) }()
	u, _ := url.Parse("http://" + ln.Addr().String())
	return u
}

func get(t *testing.T, proxy *url.URL, target string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := c.Get(target)
	if err != nil {
		t.Fatalf("GET %s via %s: %v", target, proxy, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestProxy(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "deb over http") }))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "deb over https") }))
	defer secure.Close()

	direct := serve(t, &Proxy{AllowAnyPort: true})
	if code, body := get(t, direct, plain.URL+"/pool/a.deb"); code != 200 || body != "deb over http" {
		t.Errorf("http: %d %q", code, body)
	}
	if code, body := get(t, direct, secure.URL+"/pool/a.deb"); code != 200 || body != "deb over https" {
		t.Errorf("https (CONNECT): %d %q", code, body)
	}

	// Chained through an upstream proxy, as through a local Clash.
	chained := serve(t, &Proxy{AllowAnyPort: true, Upstream: direct})
	if code, body := get(t, chained, plain.URL); code != 200 || body != "deb over http" {
		t.Errorf("chained http: %d %q", code, body)
	}
	if code, body := get(t, chained, secure.URL); code != 200 || body != "deb over https" {
		t.Errorf("chained https: %d %q", code, body)
	}

	// By default only the web ports are reachable: the relay is not a general tunnel.
	strict := serve(t, &Proxy{})
	if code, _ := get(t, strict, plain.URL); code != http.StatusForbidden {
		t.Errorf("non-web port over http: %d", code)
	}
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(strict), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if _, err := c.Get(secure.URL); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("non-web port over CONNECT: %v", err)
	}

	// Plain requests to the relay itself are refused.
	resp, err := http.Get(direct.String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-proxy request: %d", resp.StatusCode)
	}
}

func TestUpstream(t *testing.T) {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(k, "")
	}
	if u, err := Upstream("127.0.0.1:7890"); err != nil || u.String() != "http://127.0.0.1:7890" {
		t.Errorf("explicit: %v %v", u, err)
	}
	t.Setenv("HTTPS_PROXY", "http://10.0.0.2:3128")
	if u, _ := Upstream(""); u == nil || u.Host != "10.0.0.2:3128" {
		t.Errorf("env: %v", u)
	}
	if u, err := Upstream("socks5://127.0.0.1:7891"); err != nil || u.Scheme != "socks5" {
		t.Errorf("socks5: %v %v", u, err)
	}
	if _, err := Upstream("ftp://127.0.0.1:21"); err == nil {
		t.Error("ftp should be refused")
	}
}

func TestSystemProxyParsing(t *testing.T) {
	scutil := `<dictionary> {
  ExceptionsList : <array> { 0 : 127.0.0.1 }
  HTTPEnable : 1
  HTTPPort : 7890
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 7891
  HTTPSProxy : 127.0.0.1
}`
	if got := parseScutil(scutil); got != "http://127.0.0.1:7891" {
		t.Errorf("scutil: %q", got)
	}
	if got := parseScutil("<dictionary> {\n  HTTPEnable : 0\n}"); got != "" {
		t.Errorf("scutil off: %q", got)
	}
	if got := parseWindowsProxy("127.0.0.1:7890"); got != "127.0.0.1:7890" {
		t.Errorf("windows simple: %q", got)
	}
	if got := parseWindowsProxy("http=127.0.0.1:7890;https=127.0.0.1:7891;ftp=x:1"); got != "127.0.0.1:7891" {
		t.Errorf("windows per protocol: %q", got)
	}
}

func TestCheck(t *testing.T) {
	// A stand-in proxy: answers every request itself, with ok or 502.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer bad.Close()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().String()
	l.Close()
	defer func(u string) { CheckURL = u }(CheckURL)
	CheckURL = "http://example.invalid/InRelease" // plain http: the stand-in answers it without a tunnel

	for _, c := range []struct{ in, problem string }{
		{"", ""},
		{"http", "format"},
		{"http://", "format"},
		{"127.0.0.1", "port"},
		{"ftp://127.0.0.1:21", "scheme"},
		{closed, "unreachable"},
		{strings.TrimPrefix(ok.URL, "http://"), ""},
		{ok.URL, ""},
		{bad.URL, "forward"},
	} {
		if p, d := Check(c.in); p != c.problem {
			t.Errorf("Check(%q) = %q (%s), want %q", c.in, p, d, c.problem)
		}
	}
}

// noDeadlines hands out connections that, like SSH channels, cannot set deadlines.
type noDeadlines struct{ net.Listener }
type noDeadlineConn struct{ net.Conn }

func (l noDeadlines) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return noDeadlineConn{c}, nil
}
func (noDeadlineConn) SetDeadline(time.Time) error      { return errors.New("deadline not supported") }
func (noDeadlineConn) SetReadDeadline(time.Time) error  { return errors.New("deadline not supported") }
func (noDeadlineConn) SetWriteDeadline(time.Time) error { return errors.New("deadline not supported") }

// Over SSH (connections without deadlines) https through the relay used to hang in Hijack.
func TestServeBridged(t *testing.T) {
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "deb over https") }))
	defer secure.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = (&Proxy{AllowAnyPort: true}).ServeBridged(noDeadlines{ln}) }()
	u, _ := url.Parse("http://" + ln.Addr().String())
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := c.Get(secure.URL)
	if err != nil {
		t.Fatalf("https through a bridged relay: %v", err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "deb over https" {
		t.Errorf("got %q", b)
	}
}

// socks5Server is a minimal SOCKS5 proxy (no auth, CONNECT only) for the tests.
func socks5Server(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 262)
				if _, err := io.ReadFull(c, b[:2]); err != nil {
					return
				}
				if _, err := io.ReadFull(c, b[:b[1]]); err != nil {
					return
				}
				c.Write([]byte{5, 0})
				if _, err := io.ReadFull(c, b[:4]); err != nil {
					return
				}
				var host string
				switch b[3] {
				case 1:
					io.ReadFull(c, b[:4])
					host = net.IP(b[:4]).String()
				case 3:
					io.ReadFull(c, b[:1])
					n := int(b[0])
					io.ReadFull(c, b[:n])
					host = string(b[:n])
				default:
					return
				}
				io.ReadFull(c, b[:2])
				port := int(b[0])<<8 | int(b[1])
				up, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
				if err != nil {
					c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				pipe(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestUpstreamKinds(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "deb over http") }))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "deb over https") }))
	defer secure.Close()

	// A socks5 upstream, for both kinds of request.
	su, _ := url.Parse("socks5://" + socks5Server(t))
	viaSocks := serve(t, &Proxy{AllowAnyPort: true, Upstream: su})
	if code, body := get(t, viaSocks, plain.URL); code != 200 || body != "deb over http" {
		t.Errorf("socks5 http: %d %q", code, body)
	}
	if code, body := get(t, viaSocks, secure.URL); code != 200 || body != "deb over https" {
		t.Errorf("socks5 https: %d %q", code, body)
	}

	// An https upstream: TLS to the proxy, then CONNECT inside it.
	tlsProxy := httptest.NewTLSServer(&Proxy{AllowAnyPort: true})
	defer tlsProxy.Close()
	hu, _ := url.Parse(tlsProxy.URL)
	viaTLS := serve(t, &Proxy{AllowAnyPort: true, Upstream: hu, upTLS: tlsProxy.Client().Transport.(*http.Transport).TLSClientConfig})
	if code, body := get(t, viaTLS, secure.URL); code != 200 || body != "deb over https" {
		t.Errorf("https upstream: %d %q", code, body)
	}

	// https:// written for a plain proxy is caught, with the address to use instead.
	plainProxy := serve(t, &Proxy{AllowAnyPort: true})
	if p, d := Check("https://" + plainProxy.Host); p != "notTLS" || d != "http://"+plainProxy.Host {
		t.Errorf("Check(https:// on a plain proxy) = %q %q", p, d)
	}
}
