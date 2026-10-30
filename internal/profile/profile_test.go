package profile

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeRepo(t *testing.T) {
	want := "github.com/luwudynamics/xgoduck_runtime_arduino"
	for _, in := range []string{
		"https://github.com/LuwuDynamics/xgoduck_runtime_arduino",
		"https://github.com/LuwuDynamics/xgoduck_runtime_arduino.git",
		"git@github.com:LuwuDynamics/xgoduck_runtime_arduino.git",
		"ssh://git@github.com/LuwuDynamics/xgoduck_runtime_arduino",
		"github.com/LuwuDynamics/xgoduck_runtime_arduino/",
	} {
		if got := NormalizeRepo(in); got != want {
			t.Errorf("NormalizeRepo(%q) = %q", in, got)
		}
	}
	if got := NormalizeRepo("ssh://git@host:2222/a/b.git"); got != "host/a/b" {
		t.Errorf("port form: %q", got)
	}
}

func TestBuiltinsAndLookup(t *testing.T) {
	if len(Builtins()) == 0 || ByID("xgoduck") == nil {
		t.Fatal("xgoduck profile missing")
	}
	p, err := Lookup(nil, "git@github.com:LuwuDynamics/xgoduck_runtime_arduino.git", "", At{})
	if err != nil || p == nil || p.ID != "xgoduck" || p.Origin != "builtin" {
		t.Fatalf("lookup by repo: %+v %v", p, err)
	}
	if p, _ := Lookup(nil, "local:/tmp/x", "XGO Duck", At{}); p == nil || p.ID != "xgoduck" {
		t.Fatal("lookup by app name failed")
	}
	if p, _ := Lookup(nil, "https://github.com/x/y", "Other", At{}); p != nil {
		t.Fatal("generic app matched a profile")
	}
	own, err := Lookup([]byte("id: mine\nname: Mine\nhealth:\n  url: http://127.0.0.1:7000/\n"), "https://github.com/LuwuDynamics/xgoduck_runtime_arduino", "XGO Duck", At{})
	if err != nil || own.ID != "mine" || own.Origin != "repo" || own.Health.Timeout != 120 {
		t.Fatalf("repo quay.yaml must win: %+v %v", own, err)
	}
	if _, err := Parse([]byte("id: x\nfrom: 5f511b9\n")); err == nil {
		t.Fatal("a short hash in from must be rejected")
	}
	if _, err := Parse([]byte("nme: typo\n")); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
	if _, err := Parse([]byte("system:\n  check: true\n")); err == nil {
		t.Fatal("system without apply must be rejected")
	}
}

func TestScriptAndActions(t *testing.T) {
	p, err := Parse([]byte("uninstall: rm -f x\nactions:\n  - {label: Settings, url: /settings}\n"))
	if err != nil || p.Uninstall.Run != "rm -f x" || p.Actions[0].URL != "/settings" {
		t.Fatalf("plain uninstall: %+v %v", p, err)
	}
	p, err = Parse([]byte("uninstall:\n  run: rm -f x\n  description: Removes x.\n"))
	if err != nil || p.Uninstall.Run != "rm -f x" || p.Uninstall.Description != "Removes x." {
		t.Fatalf("uninstall with a description: %+v %v", p, err)
	}
	if _, err := Parse([]byte("uninstall:\n  rnu: typo\n")); err == nil {
		t.Fatal("unknown uninstall fields must be rejected")
	}
	if _, err := Parse([]byte("actions:\n  - {label: X, url: 'javascript:alert(1)'}\n")); err == nil {
		t.Fatal("an action URL must be a path or http(s)")
	}
	d := ByID("xgoduck")
	if d.Uninstall == nil || d.Uninstall.Run == "" || len(d.Actions) != 2 || d.BeforeStart.DescriptionZH == "" {
		t.Fatalf("duck profile: %+v", d)
	}
}

func TestIcon(t *testing.T) {
	for _, ok := range []string{"icon: 🦆\n", "icon: 🧑‍💻\n", "icon: '<svg viewBox=\"0 0 8 8\"><circle r=\"4\"/></svg>'\n"} {
		if _, err := Parse([]byte(ok)); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"icon: a long piece of text\n", "icon: '<img src=x>'\n", "icon: '<svg><circle r=\"4\"/>'\n"} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if d := ByID("xgoduck"); !strings.HasPrefix(d.Icon, "<svg") {
		t.Fatalf("duck profile icon: %.40q", d.Icon)
	}
}

func TestPick(t *testing.T) {
	h := func(c byte) string { return strings.Repeat(string(c), 40) }
	vs := []*Profile{{Name: "a", From: h('a')}, {Name: "b", From: h('b')}, {Name: "c", From: h('c')}}
	// The app is at a commit after b and before c.
	before := func(from string) (bool, error) { return from == h('a') || from == h('b'), nil }
	broken := func(string) (bool, error) { return false, errors.New("no history") }
	for _, c := range []struct {
		name string
		at   At
		want string
	}{
		{"history", At{Ancestor: before}, "b"},
		{"older than every version", At{Ancestor: func(string) (bool, error) { return false, nil }}, ""},
		{"no history: the version picked at install", At{Ancestor: broken, Known: h('a')}, "a"},
		{"no history, nothing known: the newest", At{}, "c"},
		{"known version gone: the newest", At{Known: h('9')}, "c"},
	} {
		got := ""
		if p := Pick(vs, c.at); p != nil {
			got = p.Name
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if p := Pick([]*Profile{{Name: "all"}}, At{Ancestor: broken}); p == nil || p.Name != "all" {
		t.Error("a profile without from is for every commit")
	}
}

// The built-in XGO-Duck profile follows the runtime's models: 3, then roulade, roller, and sit and
// the two kicks.
func TestXGODuckVersions(t *testing.T) {
	vs := versions("https://github.com/LuwuDynamics/xgoduck_runtime_arduino", "")
	var slots []int
	for _, v := range vs {
		if len(v.From) != 40 || v.Policies == nil {
			t.Fatalf("version %+v", v)
		}
		slots = append(slots, len(v.Policies.Slots))
	}
	if fmt.Sprint(slots) != "[3 4 5 8]" {
		t.Fatalf("slots per version: %v", slots)
	}
	if ByID("xgoduck") != vs[3] || len(Latest()) != 1 {
		t.Fatal("ByID and Latest give the newest version")
	}
	// 8837d5a (roller added) is at or after the third version's from and before the fourth's.
	at := At{Ancestor: func(from string) (bool, error) { return from != vs[3].From, nil }}
	if p, _ := Lookup(nil, "https://github.com/LuwuDynamics/xgoduck_runtime_arduino", "", at); p != vs[2] || p.Policies.Slot("roller") == nil {
		t.Fatalf("picked %+v", p)
	}
}
