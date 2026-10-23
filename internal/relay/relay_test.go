package relay

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
	if _, err := Upstream("socks5://127.0.0.1:7891"); err == nil {
		t.Error("socks should be refused")
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
