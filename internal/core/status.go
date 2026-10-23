package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/profile"
	"github.com/caoxg/quay/internal/remote"
	"gopkg.in/yaml.v3"
)

// Status describes a board and its user apps.
type Status struct {
	Target        string    `json:"target"`
	User          string    `json:"user"`
	Home          string    `json:"home"`
	Hostname      string    `json:"hostname"`
	Model         string    `json:"model"`
	Serial        string    `json:"serial"`
	IsUnoQ        bool      `json:"isUnoQ"`
	IPs           []IfaceIP `json:"ips"`
	AppCLI        bool      `json:"appcli"`
	AppCLIVersion string    `json:"appcliVersion"`
	AppListError  string    `json:"appListError,omitempty"`
	OS            OSInfo    `json:"os"`
	Mirror        Mirror    `json:"mirror"`
	Relay         Relay     `json:"relay"`
	// AuthorizedKeys are the base64 parts of the login user's ~/.ssh/authorized_keys, to tell
	// whether this computer's public key is on the board.
	AuthorizedKeys []string    `json:"authorizedKeys"`
	Apps           []App       `json:"apps"`
	Broken         []BrokenApp `json:"broken,omitempty"`
	CheckedAt      int64       `json:"checkedAt"`
}

// OSInfo is the board's operating system (/etc/os-release, /etc/debian_version).
type OSInfo struct {
	ID       string `json:"id"`       // debian
	Name     string `json:"name"`     // Debian GNU/Linux
	Pretty   string `json:"pretty"`   // Debian GNU/Linux 13 (trixie)
	Version  string `json:"version"`  // 13.1 (point release when known)
	Codename string `json:"codename"` // trixie
}

type IfaceIP struct {
	Iface string `json:"iface"`
	IP    string `json:"ip"`
}

type Mirror struct {
	Pip         string `json:"pip"`
	PipFallback string `json:"pipFallback,omitempty"` // pip's extra index, tried when Pip lacks a package
	UV          string `json:"uv"`
	Debian      string `json:"debian"` // where apt gets Debian packages (http://deb.debian.org/debian by default)
	// DebianFallback are the mirrors apt moves to when Debian fails, in order.
	DebianFallback []string `json:"debianFallback,omitempty"`
	Managed        bool     `json:"managed"` // set by Quay (can be turned off)
}

// Relay is whether the board's apt downloads through this computer (see Session.StartRelay).
type Relay struct {
	Installed bool `json:"installed"` // the board's apt hook is in place (quay relay on)
	Active    bool `json:"active"`    // this session is relaying right now
}

// App is one user app on the board.
type App struct {
	Folder      string           `json:"folder"`
	Path        string           `json:"path"`
	ID          string           `json:"id"` // App CLI id, e.g. user:xgoduck_runtime_arduino
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Icon        string           `json:"icon"`
	Status      string           `json:"status"` // running | stopped | starting | stopping | failed | uninitialized
	Default     bool             `json:"default"`
	Deploy      *Deployment      `json:"deploy,omitempty"`
	Data        []string         `json:"data"`
	Profile     *profile.Profile `json:"profile,omitempty"`
	SystemReady *bool            `json:"systemReady,omitempty"`
	Ports       []int            `json:"ports,omitempty"`  // app.yaml ports
	WebURL      string           `json:"webUrl,omitempty"` // the app's own console (profile web, else first port)
	// Policies are the models Quay installed in place of the stock ones, by slot.
	Policies map[string]InstalledPolicy `json:"policies,omitempty"`
}

type BrokenApp struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// Deployment is the .quay.json Quay writes into an app folder.
type Deployment struct {
	Repo       string    `json:"repo"`
	Ref        string    `json:"ref"`
	Commit     string    `json:"commit"`
	Subject    string    `json:"subject"`
	CommitDate time.Time `json:"commitDate"`
	Subdir     string    `json:"subdir,omitempty"`
	Dirty      bool      `json:"dirty,omitempty"`
	Profile    string    `json:"profile,omitempty"`
	DeployedAt time.Time `json:"deployedAt"`
	Quay       string    `json:"quay"`
}

type rawStatus struct {
	User       string   `json:"user"`
	Home       string   `json:"home"`
	Hostname   string   `json:"hostname"`
	Model      string   `json:"model"`
	Compatible []string `json:"compatible"`
	Serial     string   `json:"serial"`
	IPs        []struct {
		Ifname string `json:"ifname"`
		IP     string `json:"ip"`
	} `json:"ips"`
	AppCLI        bool   `json:"appcli"`
	AppCLIVersion string `json:"appcliVersion"`
	OS            OSInfo `json:"os"`
	AppList       *struct {
		Apps []struct {
			ID          string `json:"id"`
			DecodedID   string `json:"decodedId"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Icon        string `json:"icon"`
			Status      string `json:"status"`
			Example     bool   `json:"example"`
			Default     bool   `json:"default"`
		} `json:"apps"`
		BrokenApps []struct {
			Name  string `json:"name"`
			Error string `json:"error"`
		} `json:"brokenApps"`
	} `json:"appList"`
	AppListError   string   `json:"appListError"`
	PipIndex       string   `json:"pipIndex"`
	PipExtra       string   `json:"pipExtra"`
	UVIndex        string   `json:"uvIndex"`
	MirrorManaged  bool     `json:"mirrorManaged"`
	DebianMirror   string   `json:"debianMirror"`
	DebianFallback []string `json:"debianFallback"`
	DebianManaged  bool     `json:"debianManaged"`
	AuthorizedKeys []string `json:"authorizedKeys"`
	RelayHook      bool     `json:"relay"`
	Folders        []struct {
		Folder   string                     `json:"folder"`
		Path     string                     `json:"path"`
		Quay     *Deployment                `json:"quay"`
		QuayYAML string                     `json:"quayYaml"`
		AppYAML  string                     `json:"appYaml"`
		Data     []string                   `json:"data"`
		Policies map[string]InstalledPolicy `json:"policies"`
	} `json:"folders"`
}

// Status reads the board state (one round trip, plus one for profile checks).
func (s *Session) Status(ctx context.Context) (*Status, error) {
	var out, errb strings.Builder
	if err := s.T.Exec(ctx, "python3 -", strings.NewReader(remote.Status), &out, &errb); err != nil {
		if strings.Contains(errb.String(), "not found") {
			return nil, fmt.Errorf("python3 is missing on the board — is this an Arduino UNO Q?")
		}
		return nil, fmt.Errorf("status: %v %s", err, strings.TrimSpace(errb.String()))
	}
	var raw rawStatus
	if err := json.Unmarshal([]byte(out.String()), &raw); err != nil {
		return nil, fmt.Errorf("status: unexpected reply: %w", err)
	}
	st := &Status{
		Target: s.T.Describe(), User: raw.User, Home: raw.Home, Hostname: raw.Hostname, Model: raw.Model,
		Serial: raw.Serial, AppCLI: raw.AppCLI, AppCLIVersion: raw.AppCLIVersion, AppListError: raw.AppListError, OS: raw.OS,
		Mirror:         Mirror{Pip: raw.PipIndex, PipFallback: raw.PipExtra, UV: raw.UVIndex, Debian: raw.DebianMirror, DebianFallback: raw.DebianFallback, Managed: raw.MirrorManaged || raw.DebianManaged},
		AuthorizedKeys: append([]string{}, raw.AuthorizedKeys...),
		Relay:          Relay{Installed: raw.RelayHook, Active: s.RelayActive()},
		CheckedAt:      time.Now().UnixMilli(),
	}
	for _, c := range raw.Compatible {
		if strings.HasPrefix(c, "arduino,imola") {
			st.IsUnoQ = true
			st.Model = "Arduino UNO Q" // the device tree says "Arduino SA,Imola"
		}
	}
	for _, ip := range raw.IPs {
		st.IPs = append(st.IPs, IfaceIP{Iface: ip.Ifname, IP: ip.IP})
	}
	cli := map[string]int{}
	if raw.AppList != nil {
		for i, a := range raw.AppList.Apps {
			if !a.Example && strings.HasPrefix(a.DecodedID, "user:") {
				cli[strings.TrimPrefix(a.DecodedID, "user:")] = i
			}
		}
		for _, b := range raw.AppList.BrokenApps {
			st.Broken = append(st.Broken, BrokenApp{Name: b.Name, Error: b.Error})
		}
	}
	host := s.boardHost(st)
	var checks []string
	for _, f := range raw.Folders {
		a := App{Folder: f.Folder, Path: f.Path, Deploy: f.Quay, Data: f.Data, Status: "uninitialized", Policies: f.Policies}
		if a.Data == nil {
			a.Data = []string{}
		}
		var ay struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
			Icon        string `yaml:"icon"`
			Ports       []int  `yaml:"ports"`
		}
		_ = yaml.Unmarshal([]byte(f.AppYAML), &ay)
		a.Name, a.Description, a.Icon, a.Ports = ay.Name, ay.Description, ay.Icon, ay.Ports
		if i, ok := cli[f.Folder]; ok {
			c := raw.AppList.Apps[i]
			a.ID, a.Status, a.Default = c.DecodedID, c.Status, c.Default
			if c.Name != "" {
				a.Name = c.Name
			}
			if c.Icon != "" {
				a.Icon = c.Icon
			}
			if a.Status == "" {
				a.Status = "stopped"
			}
		}
		if a.Name == "" {
			a.Name = f.Folder
		}
		repo := ""
		if a.Deploy != nil {
			repo = a.Deploy.Repo
		}
		if p, err := profile.Lookup([]byte(f.QuayYAML), repo, ay.Name); err == nil && p != nil {
			a.Profile = p
			if p.System != nil && strings.TrimSpace(p.System.Check) != "" {
				checks = append(checks, f.Folder+"\t"+strings.ReplaceAll(p.System.Check, "\n", " ; "))
			}
		}
		a.WebURL = consoleURL(host, a.Profile, a.Ports)
		st.Apps = append(st.Apps, a)
	}
	if len(checks) > 0 {
		res, err := board.Output(ctx, s.T, remote.Script(remote.CheckAll, map[string]string{"CHECKS": strings.Join(checks, "\n")}))
		if err == nil {
			for _, l := range scanLines(res) {
				parts := strings.SplitN(l, "\t", 2)
				if len(parts) != 2 {
					continue
				}
				for i := range st.Apps {
					if st.Apps[i].Folder == parts[0] {
						ready := parts[1] == "1"
						st.Apps[i].SystemReady = &ready
					}
				}
			}
		}
	}
	return st, nil
}

// PrimaryIP is the board's main IPv4 address: Wi-Fi first, then wired, then anything else.
func (st *Status) PrimaryIP() string {
	for _, prefix := range []string{"wl", "en", "eth", ""} {
		for _, ip := range st.IPs {
			if strings.HasPrefix(ip.Iface, prefix) {
				return ip.IP
			}
		}
	}
	return ""
}

// boardHost is the address a browser on this computer can use to reach the board.
func (s *Session) boardHost(st *Status) string {
	if s.Target.Kind == board.KindSSH && s.Target.Host != "" {
		return s.Target.Host
	}
	for _, ip := range st.IPs {
		if strings.HasPrefix(ip.Iface, "wl") {
			return ip.IP
		}
	}
	if len(st.IPs) > 0 {
		return st.IPs[0].IP
	}
	return ""
}

// consoleURL is where a browser on this computer reaches the app's own web console: the profile's
// web section if it has one, else the first port the app declares in app.yaml.
func consoleURL(host string, p *profile.Profile, ports []int) string {
	port, path := 0, "/"
	if p != nil && p.Web != nil && p.Web.Port > 0 {
		port = p.Web.Port
		if p.Web.Path != "" {
			path = p.Web.Path
		}
	} else if len(ports) > 0 {
		port = ports[0]
	}
	if host == "" || port <= 0 || port > 65535 {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "http://" + hostPort(host, port) + path
}

func hostPort(h string, port int) string {
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	return h + ":" + strconv.Itoa(port)
}

// App finds an installed app by folder (or by name, case-insensitively).
func (st *Status) App(key string) (*App, error) {
	for i := range st.Apps {
		if st.Apps[i].Folder == key {
			return &st.Apps[i], nil
		}
	}
	var hit *App
	for i := range st.Apps {
		if strings.EqualFold(st.Apps[i].Name, key) || strings.EqualFold(st.Apps[i].Folder, key) {
			if hit != nil {
				return nil, fmt.Errorf("%q matches several apps; use the folder name", key)
			}
			hit = &st.Apps[i]
		}
	}
	if hit == nil {
		return nil, fmt.Errorf("no app %q on this board", key)
	}
	return hit, nil
}

// probe fetches a URL on the board; ok=false when nothing answers.
func (o *op) probe(u string) (body string, ok bool) {
	if _, err := url.Parse(u); err != nil {
		return "", false
	}
	var out strings.Builder
	if err := o.s.T.Exec(o.ctx, "python3 - "+board.Quote(u), strings.NewReader(remote.Probe), &out, nil); err != nil {
		return "", false
	}
	b := strings.TrimSpace(out.String())
	if b == "QUAY_DOWN" {
		return "", false
	}
	return b, true
}

// unsafe evaluates the profile's safety probe: a non-empty reason means "do not stop now".
func (o *op) unsafe(p *profile.Profile) string {
	if p == nil || p.Safety == nil {
		return ""
	}
	body, ok := o.probe(p.Safety.URL)
	if !ok {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return ""
	}
	if truthy(m[p.Safety.Field]) {
		if p.Safety.Message != "" {
			return p.Safety.Message
		}
		return "the app reports it is not safe to stop (" + p.Safety.Field + " is set)"
	}
	for _, c := range p.Safety.Also {
		v, hit := m[c.Field], false
		if len(c.Equals) == 0 {
			hit = truthy(v)
		} else {
			hit = slices.Contains(c.Equals, fmt.Sprint(v))
		}
		if hit {
			if c.Message != "" {
				return c.Message
			}
			return fmt.Sprintf("the app reports it is not safe to stop (%s is %v)", c.Field, v)
		}
	}
	return ""
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != "" && x != "0" && !strings.EqualFold(x, "false")
	}
	return false
}
