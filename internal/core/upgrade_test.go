package core

import (
	"os"
	"testing"
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
