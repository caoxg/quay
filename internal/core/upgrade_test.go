package core

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/upgradelist.txt is remote.UpgradeList's output from a real UNO Q, trimmed to a few
// packages: plain upgrades, a new dependency, a package from two origins, Arduino's repository,
// the desktop packages, a backports package with a space in its origin, and a pending reboot.
func TestParseUpgrades(t *testing.T) {
	b, err := os.ReadFile("testdata/upgradelist.txt")
	if err != nil {
		t.Fatal(err)
	}
	u := ParseUpgrades(string(b))
	if u.Total.Count != 9 || !u.RebootRequired {
		t.Fatalf("total %+v reboot %v", u.Total, u.RebootRequired)
	}
	if u.Arduino.Count != 3 || u.Arduino.Size != 2091564+55386796+186423376 {
		t.Errorf("arduino %+v", u.Arduino)
	}
	if u.Desktop.Count != 3 { // arduino-app-lab, chromium, chromium-common
		t.Errorf("desktop %+v", u.Desktop)
	}
	if u.Packages[0].Name != "arduino-app-lab" || u.Packages[0].Size != 186423376 {
		t.Errorf("largest first: %+v", u.Packages[0])
	}
	by := map[string]Package{}
	for _, p := range u.Packages {
		by[p.Name] = p
	}
	if p := by["arduino-app-cli"]; p.From != "0.13.0" || p.To != "0.14.0" || !p.Arduino || p.Source != "apt-repo.arduino.cc" {
		t.Errorf("app cli: %+v", p)
	}
	if p := by["libfftw3-double3"]; p.From != "" || p.To != "3.3.10-2+b1" || p.Arduino {
		t.Errorf("new dependency: %+v", p)
	}
	if p := by["libde265-0"]; p.Source != "deb.debian.org" || p.Size != 153012 {
		t.Errorf("two origins: %+v", p)
	}
	if p := by["firmware-qcom-dsp"]; p.To != "0~20260916-2~bpo13+1" || p.Size == 0 {
		t.Errorf("backports: %+v", p)
	}
	if len(u.Sources) != 2 || u.Sources[0].Size < u.Sources[1].Size {
		t.Errorf("sources, largest first: %+v", u.Sources)
	}
	for _, g := range u.Sources {
		if (g.Host == "deb.debian.org" && g.Count != 6) || (g.Host == "apt-repo.arduino.cc" && g.Count != 3) {
			t.Errorf("source %+v", g)
		}
	}
	if empty := ParseUpgrades("QUAY_URIS\nQUAY_END\n"); empty.Total.Count != 0 || empty.Packages == nil {
		t.Errorf("nothing to do: %+v", empty)
	}
}

func TestValidHostname(t *testing.T) {
	for _, ok := range []string{"duck", "duck-2", "a", "unoq-lab-3", "x1"} {
		if err := ValidHostname(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Duck", "-duck", "duck-", "du ck", "duck.local", "鸭子", "a_b",
		"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklm"} {
		if ValidHostname(bad) == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestCompareDebian(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.2.14-1qcom0.1arduino3", "1.2.16.1-1~bpo13+1", -1},
		{"1.2.16.1-1~bpo13+1", "1.2.16.1-1", -1}, // ~ sorts before the end
		{"5.2.37-2+b10", "5.2.37-2+b5", 1},
		{"1:1.0", "2.0", 1}, // epoch first
		{"1.0", "1.0-0", 0},
		{"1.0a", "1.0", 1},
		{"1.0~rc1", "1.0", -1},
		{"0.10.0", "0.9.9", 1},
		{"2.0", "2.0", 0},
	} {
		if got := CompareDebian(c.a, c.b); got != c.want {
			t.Errorf("CompareDebian(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	u := ParseUpgrades("Inst alsa-ucm-conf [1.2.16.1-1~bpo13+1] (1.2.14-1qcom0.1arduino3 Arduino:stable [all])\nInst bash [5.2.37-2+b5] (5.2.37-2+b10 Debian:13.7/stable [arm64])\n")
	if len(u.Packages) != 2 || !u.Packages[0].Downgrade || u.Packages[1].Downgrade || !u.Packages[0].ArduinoRepo {
		t.Fatalf("parsed %+v", u.Packages)
	}
}

func TestParseUpgradesLists(t *testing.T) {
	old := time.Now().Add(-150 * 24 * time.Hour).Unix()
	fresh := time.Now().Add(-6 * time.Hour).Unix()
	for _, c := range []struct {
		line  string
		stale bool
	}{
		{fmt.Sprintf("QUAY_LISTS 6 4 %d", old), true},    // just switched to a mirror
		{fmt.Sprintf("QUAY_LISTS 6 0 %d", old), true},    // never refreshed since the image
		{fmt.Sprintf("QUAY_LISTS 6 0 %d", fresh), false}, // refreshed this morning
		{"QUAY_LISTS 0 0 0", false},                      // unknown (no apt): say nothing
	} {
		u := ParseUpgrades("QUAY_URIS\nQUAY_END\n" + c.line + "\n")
		if u.ListsStale != c.stale {
			t.Errorf("%s: stale = %v", c.line, u.ListsStale)
		}
	}
}

// With "mirror cn", Debian packages download from mirror+file: lists, not from the cache.
func TestParseUpgradesMirrorList(t *testing.T) {
	out := `QUAY_MIRRORLIST /etc/apt/quay/debian.list mirror.nju.edu.cn
QUAY_MIRRORLIST /etc/apt/quay/debian-security.list mirror.nju.edu.cn
Inst chromium [150.0-1~deb13u1] (156.0.8078.47-1~deb13u1 Debian-Security:13/stable-security [arm64])
Inst bash [5.2.37-2+b5] (5.2.37-2+b10 Debian:13.5/stable [arm64])
Inst tzdata [2025b-4] (2025c-0+deb13u1 Debian:13.5/stable [all])
QUAY_URIS
'mirror+file:/etc/apt/quay/debian-security.list/pool/updates/main/c/chromium/chromium_156.0.8078.47-1%7edeb13u1_arm64.deb' chromium_156.0.8078.47-1~deb13u1_arm64.deb 79460952
'mirror+file:/etc/apt/quay/debian.list/pool/main/b/bash/bash_5.2.37-2%2bb10_arm64.deb' bash_5.2.37-2+b10_arm64.deb 1416404
QUAY_END
`
	u := ParseUpgrades(out)
	if len(u.Sources) != 2 || u.Sources[0].Host != "mirror.nju.edu.cn" || u.Sources[0].Count != 2 || u.Sources[0].Size != 79460952+1416404 {
		t.Fatalf("sources %+v", u.Sources)
	}
	if u.Sources[1].Host != "(cached)" || u.Sources[1].Count != 1 { // tzdata: no URI, so in the cache
		t.Errorf("cached %+v", u.Sources[1])
	}
}

func TestDropped(t *testing.T) {
	// What the board printed last is the message; the dropped connection is underneath.
	err := &runError{msg: "Setting up libssl3t64 (3.5.4-1~deb13u1) ...", err: errors.New("wait: remote command exited without exit status or exit signal")}
	if !dropped(err) {
		t.Error("a session cut by sshd's restart is a dropped connection")
	}
	if dropped(&runError{msg: "E: Unable to locate package foo", err: errors.New("Process exited with status 100")}) {
		t.Error("a failed apt is not a dropped connection")
	}
}

// A package dist-upgrade takes past its candidate (for a new Chromium) is marked pulled.
func TestParseUpgradesPulled(t *testing.T) {
	out := `Inst libasound2t64 [1.2.14-1] (1.2.16.1-1~bpo13+1 Debian Backports:stable-backports [arm64])
Inst alsa-ucm-conf [1.2.16.1-1~bpo13+1] (1.2.14-1qcom0.1arduino3 Arduino:stable [all])
QUAY_CAND libasound2t64 1.2.14-1
QUAY_CAND alsa-ucm-conf 1.2.14-1qcom0.1arduino3
QUAY_URIS
QUAY_END
`
	u := ParseUpgrades(out)
	got := map[string]bool{}
	for _, p := range u.Packages {
		got[p.Name] = p.Pulled
	}
	if !got["libasound2t64"] || got["alsa-ucm-conf"] {
		t.Errorf("pulled: %v", got)
	}
}

func TestParseRules(t *testing.T) {
	got := parseRules("/etc/apt/preferences.d/arduino-pins.pref arduino-unoq-config\n/etc/apt/preferences.d/local.pref\n\n")
	want := []RulesFile{{Path: "/etc/apt/preferences.d/arduino-pins.pref", Package: "arduino-unoq-config"}, {Path: "/etc/apt/preferences.d/local.pref"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("parseRules = %+v, want %+v", got, want)
	}
	if parseRules("") != nil {
		t.Error("no changed files must give none")
	}
}

func TestUpgradeSkips(t *testing.T) {
	ucm := &Package{Name: "alsa-ucm-conf", From: "1.2.16.1-1~bpo13+1", To: "1.2.14-1qcom0.1arduino3", ArduinoRepo: true, Downgrade: true}
	if (UpgradeOptions{SkipDesktop: true}).skips(ucm) || (UpgradeOptions{ArduinoOnly: true}).skips(ucm) {
		t.Error("Arduino's alsa-ucm-conf belongs to every kind of update")
	}
	if !(UpgradeOptions{SkipDesktop: true}).skips(&Package{Name: "chromium", Desktop: true}) {
		t.Error("skipping the desktop leaves chromium out")
	}
	if !(UpgradeOptions{SkipDesktop: true}).skips(&Package{Name: "libasound2t64", Pulled: true}) {
		t.Error("skipping the desktop leaves out what only it pulls in")
	}
}

func TestParsePlans(t *testing.T) {
	u := &Upgrades{Packages: []Package{{Name: "bash", Size: 100}, {Name: "chromium", Size: 900}, {Name: "libnew", Size: 50}}}
	out := `QUAY_PLAN all 0
Inst bash [5.2.37-2+b5] (5.2.37-2+b10 Debian:13.4/stable [arm64])
Inst libnew (1.0 Debian:13.4/stable [arm64])
Inst chromium [143] (155 Debian-Security:13/stable-security [arm64])
Inst librec (2.0 Debian:13.4/stable [arm64])
QUAY_PLAN skip 0
Inst bash [5.2.37-2+b5] (5.2.37-2+b10 Debian:13.4/stable [arm64])
QUAY_PLAN arduino 100
Inst bash [5.2.37-2+b5] (5.2.37-2+b10 Debian:13.4/stable [arm64])
`
	p := parsePlans(out, u)
	if a := p["all"]; a == nil || a.Count != 4 || a.Size != 1050 || len(a.Packages) != 4 {
		t.Errorf("all = %+v", a)
	}
	if s := p["skip"]; s == nil || s.Count != 1 || s.Size != 100 || s.Packages[0] != "bash" {
		t.Errorf("skip = %+v", s)
	}
	if a, ok := p["arduino"]; ok {
		t.Errorf("a failed simulation gave a plan: %+v", a)
	}
}

func TestPlanKeyAndNames(t *testing.T) {
	for opt, want := range map[UpgradeOptions]string{{}: "all", {SkipDesktop: true}: "skip", {ArduinoOnly: true}: "arduino", {ArduinoOnly: true, SkipDesktop: true, Force: true}: "arduino+skip"} {
		if got := opt.PlanKey(); got != want {
			t.Errorf("%+v: %q, want %q", opt, got, want)
		}
	}
	u := &Upgrades{Packages: []Package{{Name: "chromium", From: "1", Desktop: true}, {Name: "libnew"}, {Name: "bash", From: "1"}, {Name: "x;rm", From: "1"}, {Name: "libasound2t64", From: "1", Pulled: true}}}
	if got := strings.Join(u.names(UpgradeOptions{SkipDesktop: true}), " "); got != "bash" {
		t.Errorf("names = %q", got)
	}
	if got := strings.Join(u.names(UpgradeOptions{}), " "); got != "bash chromium libasound2t64" {
		t.Errorf("names = %q", got)
	}
}

// Arduino's packages are what App Lab's update installs (App CLI's MatchArduinoPackage), not all
// that Arduino's repository serves.
func TestAppLabPackages(t *testing.T) {
	u := ParseUpgrades(`Inst arduino-unoq-config [0.1.0] (0.2.1 Arduino:stable [arm64])
Inst libasound2t64 [1.2.14-1] (1.2.16.1-1~bpo13+1 Arduino:stable, Debian Backports:stable-backports [arm64])
Inst adbd [34.0.5-12] (34.0.5-12arduino7 Arduino:stable [arm64])
Inst bash [5.2.37-2+b5] (5.2.37-2+b10 Debian:13.7/stable [arm64])
`)
	by := map[string]Package{}
	for _, p := range u.Packages {
		by[p.Name] = p
	}
	for name, want := range map[string][2]bool{"arduino-unoq-config": {true, true}, "libasound2t64": {false, true}, "adbd": {true, true}, "bash": {false, false}} {
		if p := by[name]; p.Arduino != want[0] || p.ArduinoRepo != want[1] {
			t.Errorf("%s: Arduino %v ArduinoRepo %v, want %v", name, p.Arduino, p.ArduinoRepo, want)
		}
	}
	if got := u.names(UpgradeOptions{ArduinoOnly: true}); strings.Join(got, " ") != "adbd arduino-unoq-config" {
		t.Errorf("Arduino's packages only: %v", got)
	}
}
