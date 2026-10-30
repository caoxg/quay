package board

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestParseTarget(t *testing.T) {
	cases := map[string]Target{
		"duck.local":               {Kind: KindSSH, Host: "duck.local", User: "arduino", Port: 22},
		"me@10.0.0.2:2200":         {Kind: KindSSH, Host: "10.0.0.2", User: "me", Port: 2200},
		"ssh://arduino@unoq.local": {Kind: KindSSH, Host: "unoq.local", User: "arduino", Port: 22},
		"arduino@[fe80::1]:22":     {Kind: KindSSH, Host: "fe80::1", User: "arduino", Port: 22},
		"adb":                      {Kind: KindADB},
		"adb:2a1f9c03":             {Kind: KindADB, Serial: "2a1f9c03"},
	}
	for in, want := range cases {
		got, err := ParseTarget(in)
		if err != nil || got != want {
			t.Errorf("ParseTarget(%q) = %+v, %v", in, got, err)
		}
		if back, _ := ParseTarget(got.String()); back != got {
			t.Errorf("round trip %q -> %q", in, got.String())
		}
	}
	if _, err := ParseTarget("a@b:99999"); err == nil {
		t.Error("bad port accepted")
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{"": "''", "plain/path-1.0": "plain/path-1.0", "it's": `'it'\''s'`, "a b": "'a b'", "$(x)": "'$(x)'"} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestGenerateKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows
	if _, _, err := LocalPublicKey(""); err == nil {
		t.Fatal("a fresh home should have no key")
	}
	path, line, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "ssh-ed25519 AAAA") || KeyBlob(line) == "" {
		t.Fatalf("public key line: %q", line)
	}
	if p, l, err := LocalPublicKey(""); err != nil || p != path || l != line {
		t.Fatalf("LocalPublicKey after generating: %q %q %v", p, l, err)
	}
	priv, err := os.ReadFile(strings.TrimSuffix(path, ".pub"))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		t.Fatalf("the private key does not parse: %v", err)
	}
	if KeyBlob(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != KeyBlob(line) {
		t.Fatal("the .pub does not belong to the private key")
	}
	if fi, _ := os.Stat(strings.TrimSuffix(path, ".pub")); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode %v", fi.Mode().Perm())
	}
	if _, _, err := GenerateKey(); err == nil {
		t.Fatal("an existing key must not be replaced")
	}
}

func TestParseADBInfo(t *testing.T) {
	fresh := parseADBInfo("uno-q\r\nArduino UnoQ\r\ninactive\r\narduino L 2026-05-28 0 99999 7 -1\r\n")
	if fresh != (ADBInfo{Hostname: "uno-q", Model: "Arduino UnoQ", SSH: "inactive", Password: "L"}) {
		t.Errorf("fresh board: %+v", fresh)
	}
	if set := parseADBInfo("duck\nArduino UnoQ\nactive\narduino P 2026-10-27 0 99999 7 -1\n"); set.SSH != "active" || set.Password != "P" {
		t.Errorf("set-up board: %+v", set)
	}
	if odd := parseADBInfo("phone\n"); odd.Hostname != "phone" || odd.SSH != "" || odd.Password != "" {
		t.Errorf("a device without systemd: %+v", odd)
	}
}
