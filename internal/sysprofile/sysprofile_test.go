package sysprofile

import (
	"strings"
	"testing"
)

func TestBuiltinDuckOS(t *testing.T) {
	s := Builtin()
	if len(s.Problems) > 0 || len(s.Profiles) == 0 || s.Updated == "" {
		t.Fatalf("built-in set: %+v", s)
	}
	duckOS := Facts{Files: map[string]FileFact{
		"/etc/duck-release.json": {Exists: true, JSON: map[string]any{"release": "0.1.0-rc6", "base_version": "20260528-558"}},
	}}
	p := Identify(s.Profiles, duckOS)
	if p == nil || p.ID != "duck-os" {
		t.Fatalf("Duck OS not recognised: %+v", p)
	}
	if d := p.Info(duckOS); d.Version != "0.1.0-rc6" || d.Base != "20260528-558" || d.Name != "Duck OS" {
		t.Fatalf("info: %+v", d)
	}
	if Identify(s.Profiles, Facts{Files: map[string]FileFact{}}) != nil {
		t.Fatal("the stock image was taken for a known system")
	}
	exists, keys := Checks(s.Profiles)
	if len(exists) == 0 || exists[0] != "/etc/duck-release.json" || strings.Join(keys["/etc/duck-release.json"], ",") != "release,base_version" {
		t.Fatalf("checks: %v %v", exists, keys)
	}

	// Duck Setup's hotspot: shown in the network dialog while it comes up by itself.
	duckOS.Profiles = []NMProfile{
		{UUID: "u-home", ID: "hyperion", SSID: "hyperion", Mode: "infrastructure", Autoconnect: true},
		{UUID: "u-hot", ID: "Duck-Setup-hotspot-1a2b3c4d", SSID: "XGO-Duck", Mode: "ap", Autoconnect: true},
	}
	fs := p.Findings(duckOS, "network")
	if len(fs) != 1 || fs[0].ID != "duck-setup-hotspot" || len(fs[0].Match) != 1 || fs[0].Match[0] != "u-hot" ||
		!strings.Contains(fs[0].TextZh, "「XGO-Duck」") || fs[0].Action == nil || fs[0].Action.Do != DoNMAutoconnectOff {
		t.Fatalf("findings: %+v", fs)
	}
	duckOS.Profiles[1].Autoconnect = false
	if fs := p.Findings(duckOS, "network"); len(fs) != 0 {
		t.Fatalf("a hotspot that no longer comes up by itself: %+v", fs)
	}
	if fs := p.Findings(Facts{Files: duckOS.Files}, "network"); len(fs) != 0 {
		t.Fatal("a network note without the saved profiles read")
	}
}

func TestParseRejects(t *testing.T) {
	for name, y := range map[string]string{
		"bad id":    "id: Duck OS\nname: x\ndetect: {files: [/etc/x]}\n",
		"no detect": "id: x\nname: x\n",
		"relative":  "id: x\nname: x\ndetect: {files: [etc/x]}\n",
		"dotdot":    "id: x\nname: x\ndetect: {files: [/etc/../x]}\n",
		"shell":     "id: x\nname: x\ndetect: {files: ['/etc/x;rm -rf /']}\n",
	} {
		if p, _ := Parse([]byte(y)); p != nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p, errs := Parse([]byte(`id: x
name: X
detect: {files: [/etc/x]}
notes:
  - {id: a, show_in: network, text: t, when: {nm_profile: {id_prefix: P}}, action: {do: rm_rf}}
  - {id: b, show_in: elsewhere, text: t}
  - {id: c, show_in: overview, text: t, action: {do: nm_delete}}
`))
	if p == nil || len(p.Notes) != 1 || p.Notes[0].Action != nil || len(errs) != 3 {
		t.Fatalf("notes: %+v %v", p, errs)
	}
}
