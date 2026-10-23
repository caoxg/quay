package core

import (
	"testing"

	"github.com/caoxg/quay/internal/profile"
)

func TestConsoleURL(t *testing.T) {
	web := &profile.Profile{Web: &profile.Web{Port: 9527}}
	webPath := &profile.Profile{Web: &profile.Web{Port: 8080, Path: "ui/"}}
	noWeb := &profile.Profile{}
	cases := []struct {
		host  string
		p     *profile.Profile
		ports []int
		want  string
	}{
		{"duck.local", web, []int{7000}, "http://duck.local:9527/"}, // profile wins over app.yaml
		{"duck.local", webPath, nil, "http://duck.local:8080/ui/"},
		{"192.168.1.5", noWeb, []int{7000, 7001}, "http://192.168.1.5:7000/"}, // first app.yaml port
		{"192.168.1.5", nil, []int{7000}, "http://192.168.1.5:7000/"},
		{"fe80::1", nil, []int{7000}, "http://[fe80::1]:7000/"},
		{"duck.local", nil, nil, ""},      // nothing to open
		{"", web, nil, ""},                // unknown address
		{"duck.local", nil, []int{0}, ""}, // bad port
		{"duck.local", nil, []int{70000}, ""},
	}
	for _, c := range cases {
		if got := consoleURL(c.host, c.p, c.ports); got != c.want {
			t.Errorf("consoleURL(%q, %+v, %v) = %q, want %q", c.host, c.p, c.ports, got, c.want)
		}
	}
}
