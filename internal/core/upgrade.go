package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/remote"
)

// Package is one pending package upgrade (or a new package an upgrade pulls in).
type Package struct {
	Name    string `json:"name"`
	From    string `json:"from,omitempty"` // installed version; empty for a new dependency
	To      string `json:"to"`
	Size    int64  `json:"size"`   // download size in bytes
	Source  string `json:"source"` // download host, e.g. deb.debian.org
	Arduino bool   `json:"arduino"`
	Desktop bool   `json:"desktop"` // part of the on-board desktop (App Lab, Chromium)
}

// Group sums some packages.
type Group struct {
	Count int   `json:"count"`
	Size  int64 `json:"size"`
}

// SourceGroup sums the packages downloaded from one host.
type SourceGroup struct {
	Host string `json:"host"`
	Group
}

// Upgrades is what a system update would download and install.
type Upgrades struct {
	Packages       []Package     `json:"packages"` // largest first
	Total          Group         `json:"total"`
	Arduino        Group         `json:"arduino"`
	Desktop        Group         `json:"desktop"`
	Sources        []SourceGroup `json:"sources"`
	RebootRequired bool          `json:"rebootRequired"`
	Running        []string      `json:"running"` // apps that will be interrupted
	AppCLIVersion  string        `json:"appcliVersion"`
	CheckedAt      int64         `json:"checkedAt"`
}

// desktopPackages are installed for using the board as a computer (monitor, keyboard): the
// on-board App Lab and the browser it runs in. A board used from another computer can skip them.
var desktopPackages = map[string]bool{
	"arduino-app-lab": true, "chromium": true, "chromium-common": true, "chromium-l10n": true,
	"chromium-sandbox": true, "chromium-driver": true, "chromium-shell": true,
}

var instRe = regexp.MustCompile(`^Inst (\S+) (?:\[([^\]]*)\] )?\((\S+) ([^\[]*?)\s*\[[^\]]*\]\)`)

// ParseUpgrades reads the output of remote.UpgradeList.
func ParseUpgrades(out string) *Upgrades {
	u := &Upgrades{Packages: []Package{}, Sources: []SourceGroup{}, CheckedAt: time.Now().UnixMilli()}
	byName := map[string]*Package{}
	section := "inst"
	for _, l := range scanLines(out) {
		switch l {
		case "QUAY_URIS":
			section = "uris"
			continue
		case "QUAY_END":
			section = ""
			continue
		case "QUAY_REBOOT":
			u.RebootRequired = true
			continue
		}
		switch section {
		case "inst":
			m := instRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			origin := strings.TrimSpace(strings.SplitN(m[4], ",", 2)[0])
			p := &Package{Name: m[1], From: m[2], To: m[3], Desktop: desktopPackages[m[1]],
				Arduino: strings.HasPrefix(strings.ToLower(origin), "arduino")}
			byName[p.Name] = p
		case "uris":
			// 'http://deb.debian.org/debian/pool/main/b/bash/bash_5.2.37-2%2bb10_arm64.deb' bash_5.2.37-2+b10_arm64.deb 1416404 SHA256:…
			f := strings.Fields(l)
			if len(f) < 3 {
				continue
			}
			name, _, _ := strings.Cut(f[1], "_")
			p := byName[name]
			if p == nil {
				continue
			}
			p.Size, _ = strconv.ParseInt(f[2], 10, 64)
			uri := strings.Trim(f[0], "'")
			if _, rest, ok := strings.Cut(uri, "://"); ok {
				p.Source, _, _ = strings.Cut(rest, "/")
			}
			if strings.Contains(p.Source, "arduino.cc") {
				p.Arduino = true
			}
		}
	}
	sources := map[string]*SourceGroup{}
	for _, p := range byName {
		u.Packages = append(u.Packages, *p)
		u.Total.Count++
		u.Total.Size += p.Size
		if p.Arduino {
			u.Arduino.Count++
			u.Arduino.Size += p.Size
		}
		if p.Desktop {
			u.Desktop.Count++
			u.Desktop.Size += p.Size
		}
		host := p.Source
		if host == "" {
			host = "(cached)"
		}
		g := sources[host]
		if g == nil {
			g = &SourceGroup{Host: host}
			sources[host] = g
		}
		g.Count++
		g.Size += p.Size
	}
	sort.Slice(u.Packages, func(i, j int) bool {
		if u.Packages[i].Size != u.Packages[j].Size {
			return u.Packages[i].Size > u.Packages[j].Size
		}
		return u.Packages[i].Name < u.Packages[j].Name
	})
	for _, g := range sources {
		u.Sources = append(u.Sources, *g)
	}
	sort.Slice(u.Sources, func(i, j int) bool { return u.Sources[i].Size > u.Sources[j].Size })
	return u
}

// CheckUpgrades lists what a system update would install. refresh first updates the package lists
// (`apt-get update`, which the board image allows without a password).
func (s *Session) CheckUpgrades(ctx context.Context, sink Sink, refresh bool) (*Upgrades, error) {
	o := s.op(ctx, sink)
	if refresh {
		if err := o.step("refresh", "Refresh the package lists", func() error {
			return o.sudoN("Refresh the package lists", "apt-get update", aptNoise)
		}); err != nil {
			return nil, err
		}
	}
	var u *Upgrades
	err := o.step("list", "List the available updates", func() error {
		out, err := o.quiet(remote.Script(remote.UpgradeList, nil))
		if err != nil {
			return err
		}
		u = ParseUpgrades(out)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if st, err := s.Status(ctx); err == nil {
		u.AppCLIVersion = st.AppCLIVersion
		for _, a := range st.Apps {
			if active(a.Status) {
				u.Running = append(u.Running, a.Name)
			}
		}
	}
	u.Running = append([]string{}, u.Running...)
	return u, nil
}

// UpgradeOptions choose what a system update installs.
type UpgradeOptions struct {
	ArduinoOnly bool `json:"arduinoOnly"` // only Arduino's packages (App CLI, router, …)
	SkipDesktop bool `json:"skipDesktop"` // leave the on-board App Lab and Chromium alone
	Force       bool `json:"force"`       // ignore the apps' safety checks
	// DownloadOnly fetches the packages into the board's apt cache and stops; a later update
	// installs them without downloading again. Running apps are not touched.
	DownloadOnly bool `json:"downloadOnly"`
}

// UpgradeResult reports a finished system update.
type UpgradeResult struct {
	Remaining      int    `json:"remaining"`
	RebootRequired bool   `json:"rebootRequired"`
	AppCLIVersion  string `json:"appcliVersion"`
	Downloaded     bool   `json:"downloaded,omitempty"` // DownloadOnly: the packages are in the board's cache
}

// Progress is the payload of "progress" events: how far a step is (Percent 0–100), as bytes or
// packages done out of a total.
type Progress struct {
	Percent float64 `json:"percent"`
	Done    int64   `json:"done"`
	Total   int64   `json:"total"`
	Unit    string  `json:"unit"` // bytes | packages
}

// progress emits a step's progress, at most every half percent.
type progress struct {
	o    *op
	step string
	last float64
}

func (p *progress) set(pr Progress) {
	pr.Percent = max(0, min(pr.Percent, 100))
	if pr.Percent < 100 && pr.Percent-p.last < 0.5 {
		return
	}
	p.last = pr.Percent
	p.o.emit(Event{Type: "progress", Step: p.step, Data: pr})
}

// Upgrade updates the board's system packages in two steps: Quay first downloads them into the
// board's apt cache (with progress), then installs them: through the board's own
// `arduino-app-cli system update` (what App Lab runs), or, with SkipDesktop, through the same apt
// commands without the desktop packages. Both steps run detached on the board, so a dropped
// connection does not interrupt dpkg; running it again rejoins them.
func (s *Session) Upgrade(ctx context.Context, sink Sink, opt UpgradeOptions) (*UpgradeResult, error) {
	o := s.op(ctx, sink)
	st, err := s.Status(ctx)
	if err != nil {
		return nil, err
	}
	if err := o.step("safety", "Check it is safe to interrupt the running apps", func() error {
		running := false
		for i := range st.Apps {
			if a := &st.Apps[i]; active(a.Status) {
				running = true
				if err := o.guardStop(a, opt.Force); err != nil {
					return err
				}
			}
		}
		if !running || opt.DownloadOnly {
			return errSkip
		}
		o.warn("Running apps may restart during the update (it restarts the router and Docker when they are upgraded)")
		return nil
	}); err != nil {
		return nil, err
	}

	// Refreshing first also tells whether the image lets us run apt without a password.
	needPassword := false
	if err := o.step("refresh", "Refresh the package lists", func() error {
		_, err := o.run(remote.Script("sudo -n apt-get update </dev/null", nil), aptNoise)
		if err != nil && strings.Contains(err.Error(), "password is required") {
			needPassword = true
			return o.sudo("Refresh the package lists", "apt-get update", nil)
		}
		return err
	}); err != nil {
		return nil, err
	}

	out, err := o.quiet(remote.Script(remote.UpgradeList, nil))
	if err != nil {
		return nil, err
	}
	u := ParseUpgrades(out)
	var names []string
	var fetch int64    // bytes still to download
	var installs int64 // packages that change
	for _, p := range u.Packages {
		switch {
		case p.From == "": // a new dependency: comes in with its parent
		case (opt.ArduinoOnly && !p.Arduino) || (opt.SkipDesktop && p.Desktop):
			continue
		default:
			names = append(names, board.Quote(p.Name))
		}
		fetch += p.Size
		installs++
	}
	if len(names) == 0 {
		o.info("Nothing to update", nil)
		return &UpgradeResult{}, nil
	}
	sort.Strings(names)
	list := strings.Join(names, " ")

	if err := o.step("download", fmt.Sprintf("Download the updates (%s)", human(int(fetch))), func() error {
		if fetch == 0 {
			o.info("Already downloaded", nil)
			return nil
		}
		pr := &progress{o: o, step: "download", last: -1}
		filter := func(l string) bool {
			// dlstatus:<n>:<percent>:<text> from APT::Status-Fd: percent of the bytes to fetch.
			if rest, ok := strings.CutPrefix(l, "dlstatus:"); ok {
				if f := strings.SplitN(rest, ":", 3); len(f) == 3 {
					if pc, err := strconv.ParseFloat(f[1], 64); err == nil {
						pr.set(Progress{Percent: pc, Done: int64(pc / 100 * float64(fetch)), Total: fetch, Unit: "bytes"})
					}
				}
				return false
			}
			return !strings.HasPrefix(l, "pmstatus:") && aptNoise(l)
		}
		cmd := "apt-get install --only-upgrade -y -d -o APT::Status-Fd=1 " + list
		if needPassword {
			return o.sudoFiltered("Download the updates", cmd+" </dev/null", filter)
		}
		_, err := o.run(remote.Script(remote.UpgradeJob, map[string]string{"CMD": "sudo -n " + cmd}), func(l string) bool {
			return l != "QUAY_ATTACH" && filter(l)
		})
		if err == nil {
			pr.set(Progress{Percent: 100, Done: fetch, Total: fetch, Unit: "bytes"})
		}
		if err != nil && o.ctx.Err() != nil {
			return errors.New("stopped watching; the download keeps running on the board")
		}
		return err
	}); err != nil {
		return nil, err
	}
	if opt.DownloadOnly {
		o.info(fmt.Sprintf("%d packages are downloaded; install them when the board is free", installs), nil)
		return &UpgradeResult{Remaining: u.Total.Count, Downloaded: true, RebootRequired: u.RebootRequired}, nil
	}

	// Install: progress by apt's "Unpacking …" and "Setting up …" lines, two per package.
	pr := &progress{o: o, step: "upgrade", last: -1}
	var steps int64
	count := func(l string) {
		if strings.HasPrefix(l, "Unpacking ") || strings.HasPrefix(l, "Setting up ") {
			steps++
			pr.set(Progress{Percent: min(99, 100*float64(steps)/float64(2*installs)), Done: min(installs, steps/2), Total: installs, Unit: "packages"})
		}
	}
	install := "apt-get install --only-upgrade -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold " + list
	if needPassword {
		if err := o.step("upgrade", "Install the updates", func() error {
			return o.sudoFiltered("Install the updates", install+" </dev/null", func(l string) bool { count(l); return aptNoise(l) })
		}); err != nil {
			return nil, err
		}
		return s.upgradeResult(o)
	}
	cmd := "sudo -n " + install + " && { sudo -n needrestart -r a || true; } && { sudo -n apt-get clean -y || true; }"
	if !opt.SkipDesktop {
		cmd = "arduino-app-cli system update --yes"
		if opt.ArduinoOnly {
			cmd += " --only-arduino"
		}
	}
	if err := o.step("upgrade", "Install the updates", func() error {
		completed := false
		_, err := o.run(remote.Script(remote.UpgradeJob, map[string]string{"CMD": cmd}), func(l string) bool {
			if l == "QUAY_ATTACH" {
				o.info("An update started earlier is still running on the board; following it", nil)
				return false
			}
			if strings.Contains(l, "Upgrade completed") {
				completed = true
			}
			count(strings.TrimSpace(l))
			return aptNoise(l)
		})
		if err != nil && o.ctx.Err() != nil {
			return errors.New("stopped watching; the update keeps running on the board")
		}
		// arduino-app-cli ends by restarting its own daemon (needrestart), which cuts its progress
		// stream: it prints "Upgrade completed. Restarting ...", then an unformatted error, and dies
		// of SIGTERM (exit 143). The update itself is done; upgradeResult checks what is left.
		if err != nil && completed {
			o.info("Done; the board's services were restarted to pick up the new versions", nil)
			err = nil
		}
		if err == nil {
			pr.set(Progress{Percent: 100, Done: installs, Total: installs, Unit: "packages"})
		}
		return err
	}); err != nil {
		return nil, err
	}
	return s.upgradeResult(o)
}

func (s *Session) upgradeResult(o *op) (*UpgradeResult, error) {
	r := &UpgradeResult{}
	_ = o.step("result", "Check the result", func() error {
		out, err := o.quiet(remote.Script(remote.UpgradeList, nil))
		if err != nil {
			return err
		}
		u := ParseUpgrades(out)
		r.Remaining, r.RebootRequired = u.Total.Count, u.RebootRequired
		if v, err := o.quiet("arduino-app-cli version 2>/dev/null | head -1"); err == nil {
			r.AppCLIVersion = strings.TrimSpace(v)
		}
		return nil
	})
	if r.RebootRequired {
		o.warn("Restart the board to finish the update (a new kernel or firmware was installed)")
	}
	return r, nil
}

// aptNoise hides apt's progress chatter from the activity log.
func aptNoise(l string) bool {
	t := strings.TrimSpace(l)
	return t != "" && !strings.HasPrefix(t, "(Reading database") && !strings.HasPrefix(t, "Reading package lists") &&
		!strings.HasPrefix(t, "Building dependency tree") && !strings.HasPrefix(t, "Reading state information")
}

// sudoN runs a root command the board image allows without a password (the update and rename
// commands App Lab uses); on an image that does not, it asks for the password like any root step.
func (o *op) sudoN(title, cmd string, filter func(string) bool) error {
	_, err := o.run(remote.Script("sudo -n "+cmd+" </dev/null", nil), filter)
	if err != nil && strings.Contains(err.Error(), "password is required") {
		return o.sudo(title, cmd, nil)
	}
	return err
}
