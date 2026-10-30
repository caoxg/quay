package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caoxg/quay/internal/remote"
)

// Package is one pending package upgrade (or a new package an upgrade pulls in).
type Package struct {
	Name   string `json:"name"`
	From   string `json:"from,omitempty"` // installed version; empty for a new dependency
	To     string `json:"to"`
	Size   int64  `json:"size"`   // download size in bytes
	Source string `json:"source"` // download host, e.g. deb.debian.org
	// Arduino: one of the packages App Lab's update installs. It names only those (App CLI's
	// MatchArduinoPackage: arduino-*, and Arduino's adbd), not everything Arduino's repository has.
	Arduino bool `json:"arduino"`
	// ArduinoRepo: the version comes from Arduino's apt repository (libasound2t64 does too).
	ArduinoRepo bool `json:"arduinoRepo,omitempty"`
	Desktop     bool `json:"desktop"` // part of the on-board desktop (App Lab, Chromium)
	// Downgrade: To is a lower version than From. apt only does that when a pin of priority
	// above 1000 asks for it, like Arduino's arduino-pins.pref for its patched builds.
	Downgrade bool `json:"downgrade,omitempty"`
	// Pulled: To is not the package's candidate (the version its priority picks); dist-upgrade
	// takes it there only because another upgrade needs it (libasound2 from backports for a new
	// Chromium). An update that leaves some packages out must not name it, or it drags along
	// what it breaks (Arduino's pinned alsa-ucm-conf).
	Pulled bool `json:"pulled,omitempty"`
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
	// The board's package lists: configured sources without one (just switched to a mirror, say),
	// the newest repository date among them (Unix seconds; 0 unknown), and whether they are too
	// old or incomplete to say "up to date" without refreshing them first.
	ListsMissing int   `json:"listsMissing,omitempty"`
	ListsDate    int64 `json:"listsDate,omitempty"`
	ListsStale   bool  `json:"listsStale,omitempty"`
	// Plans: what an update installs with each choice of options, by UpgradeOptions.PlanKey. A
	// choice apt could not work out is missing.
	Plans map[string]*Plan `json:"plans,omitempty"`
}

// Plan is what an update with some options installs, as apt works it out from the packages Quay
// names: the new dependencies they need come in with them, and those only the left-out packages
// need stay out.
type Plan struct {
	Count    int      `json:"count"`
	Size     int64    `json:"size"`     // still to download; packages in the board's cache count 0
	Packages []string `json:"packages"` // names, new dependencies included
}

// MaxListsAge is how old the newest repository date in the board's package lists may be before
// Quay refreshes them rather than trust them: Debian publishes its suites several times a day.
const MaxListsAge = 3 * 24 * time.Hour

// desktopPackages are installed for using the board as a computer (monitor, keyboard): the
// on-board App Lab and the browser it runs in. A board used from another computer can skip them.
var desktopPackages = map[string]bool{
	"arduino-app-lab": true, "chromium": true, "chromium-common": true, "chromium-l10n": true,
	"chromium-sandbox": true, "chromium-driver": true, "chromium-shell": true,
}

// appLabPackage tells whether App Lab's update installs a package: App CLI's MatchArduinoPackage.
func appLabPackage(name, to string) bool {
	return strings.HasPrefix(name, "arduino-") || (name == "adbd" && strings.Contains(to, "arduino"))
}

var instRe = regexp.MustCompile(`^Inst (\S+) (?:\[([^\]]*)\] )?\((\S+) ([^\[]*?)\s*\[[^\]]*\]\)`)

// ParseUpgrades reads the output of remote.UpgradeList.
func ParseUpgrades(out string) *Upgrades {
	u := &Upgrades{Packages: []Package{}, Sources: []SourceGroup{}, CheckedAt: time.Now().UnixMilli()}
	byName := map[string]*Package{}
	mirrors := map[string]string{} // mirror+file:/etc/apt/quay/NAME.list → its first mirror's host
	cands := map[string]string{}   // package → its candidate version
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
		if rest, ok := strings.CutPrefix(l, "QUAY_CAND "); ok {
			if f := strings.Fields(rest); len(f) == 2 && f[1] != "(none)" { // (none): apt knows no candidate
				cands[f[0]] = f[1]
			}
			continue
		}
		if rest, ok := strings.CutPrefix(l, "QUAY_MIRRORLIST "); ok {
			if f := strings.Fields(rest); len(f) == 2 {
				mirrors["mirror+file:"+f[0]] = f[1]
			}
			continue
		}
		if rest, ok := strings.CutPrefix(l, "QUAY_LISTS "); ok {
			var targets int
			if _, err := fmt.Sscan(rest, &targets, &u.ListsMissing, &u.ListsDate); err == nil && targets > 0 {
				u.ListsStale = u.ListsMissing > 0 || u.ListsDate == 0 || time.Since(time.Unix(u.ListsDate, 0)) > MaxListsAge
			}
			continue
		}
		switch section {
		case "inst":
			m := instRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			origin := strings.TrimSpace(strings.SplitN(m[4], ",", 2)[0])
			p := &Package{Name: m[1], From: m[2], To: m[3], Desktop: desktopPackages[m[1]], Arduino: appLabPackage(m[1], m[3]),
				ArduinoRepo: strings.HasPrefix(strings.ToLower(origin), "arduino")}
			p.Downgrade = p.From != "" && CompareDebian(p.To, p.From) < 0
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
			if rest, ok := strings.CutPrefix(uri, "mirror+file:"); ok { // resolved below
				if i := strings.Index(rest, ".list/"); i >= 0 {
					p.Source = "mirror+file:" + rest[:i+len(".list")]
				}
			} else if _, rest, ok := strings.Cut(uri, "://"); ok {
				p.Source, _, _ = strings.Cut(rest, "/")
			}
			if strings.Contains(p.Source, "arduino.cc") {
				p.ArduinoRepo = true
			}
		}
	}
	for _, p := range byName {
		if c := cands[p.Name]; c != "" && c != p.To {
			p.Pulled = true
		}
		if strings.HasPrefix(p.Source, "mirror+file:") {
			if h := mirrors[p.Source]; h != "" {
				p.Source = h
			} else {
				p.Source = strings.TrimSuffix(path.Base(p.Source), ".list") + " mirror list"
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
		if err := o.clock(); err != nil {
			return nil, err
		}
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
		if u.Total.Count > 0 {
			u.Plans = o.plans(u, planChoices...)
		}
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

// CheckUpgradesFresh is CheckUpgrades for a look nobody asked for (the board page opening): when
// the package lists are missing or old it refreshes them first, but only as the board allows
// without a password and with its clock right, so it never asks anything. When it cannot, the
// result still says the lists are stale.
func (s *Session) CheckUpgradesFresh(ctx context.Context, sink Sink) (*Upgrades, error) {
	u, err := s.CheckUpgrades(ctx, sink, false)
	if err != nil || !u.ListsStale {
		return u, err
	}
	o := s.op(ctx, sink)
	if c, err := o.boardClock(); err != nil || (!c.Synced && c.Skew.Abs() >= MaxClockSkew) {
		return u, nil // apt would refuse the lists; the dialog sets the clock when asked
	}
	if err := o.step("refresh", "Refresh the package lists", func() error {
		_, err := o.run(remote.Script("sudo -n apt-get update </dev/null", nil), aptNoise)
		return err
	}); err != nil {
		return u, nil
	}
	return s.CheckUpgrades(ctx, sink, false)
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
	// Rules: the update changed apt's version rules, and they change more of the packages it
	// picks. Quay does not install those by itself: whoever started the update decides.
	Rules *RulesChange `json:"rules,omitempty"`
}

// RulesChange is what an update did to apt's version rules (apt preferences) and what the new
// rules change. On the UNO Q, arduino-unoq-config 0.2 brings /etc/apt/preferences.d/arduino-pins.pref,
// which trades the alsa-ucm-conf from Debian backports that the same update installed for
// Arduino's patched build: a second round, shown as a downgrade.
type RulesChange struct {
	Files    []RulesFile `json:"files"`
	Packages []Package   `json:"packages"`
}

// RulesFile is an apt preference file the update changed, with the package that brought it.
type RulesFile struct {
	Path    string `json:"path"`
	Package string `json:"package,omitempty"`
}

// parseRules reads remote.PrefsChanged: "path package" lines (package empty when none owns it).
func parseRules(out string) []RulesFile {
	var files []RulesFile
	for _, l := range scanLines(out) {
		if f := strings.Fields(l); len(f) > 0 {
			r := RulesFile{Path: f[0]}
			if len(f) > 1 {
				r.Package = f[1]
			}
			files = append(files, r)
		}
	}
	return files
}

// skips tells whether an update with opt leaves p out. Arduino's packages only are App Lab's, plus a
// downgrade back to Arduino's build (alsa-ucm-conf, after an update that took Debian's): App Lab's
// update never leaves it, so its boards keep that build all along.
func (opt UpgradeOptions) skips(p *Package) bool {
	return (opt.ArduinoOnly && !p.Arduino && !(p.Downgrade && p.ArduinoRepo)) || (opt.SkipDesktop && (p.Desktop || p.Pulled))
}

// PlanKey names opt's choice of packages in Upgrades.Plans: "all", "arduino", "skip" (the desktop)
// or "arduino+skip".
func (opt UpgradeOptions) PlanKey() string {
	switch {
	case opt.ArduinoOnly && opt.SkipDesktop:
		return "arduino+skip"
	case opt.ArduinoOnly:
		return "arduino"
	case opt.SkipDesktop:
		return "skip"
	}
	return "all"
}

// planChoices are the choices a check works out: those the update dialog offers.
var planChoices = []UpgradeOptions{{}, {SkipDesktop: true}, {ArduinoOnly: true}, {ArduinoOnly: true, SkipDesktop: true}}

// names are the packages an update with opt names to apt: the installed ones it upgrades. New
// dependencies come in with them.
func (u *Upgrades) names(opt UpgradeOptions) []string {
	var names []string
	for i := range u.Packages {
		if p := &u.Packages[i]; p.From != "" && !opt.skips(p) && pkgName.MatchString(p.Name) {
			names = append(names, p.Name)
		}
	}
	sort.Strings(names)
	return names
}

var pkgName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.:-]*$`)

// plans asks apt what an update with each of opts installs (remote.UpgradePlan), all at once. A
// plan apt cannot work out is missing from the result; so are all of them when the board cannot
// be asked.
func (o *op) plans(u *Upgrades, opts ...UpgradeOptions) map[string]*Plan {
	plans := map[string]*Plan{}
	keyOf := map[string]string{} // the names, joined → the key of the first choice naming them
	alias := map[string]string{} // a choice naming the same packages as an earlier one → its key
	var lines []string
	for _, opt := range opts {
		k, names := opt.PlanKey(), u.names(opt)
		if len(names) == 0 {
			plans[k] = &Plan{Packages: []string{}}
			continue
		}
		j := strings.Join(names, " ")
		if first, ok := keyOf[j]; ok {
			alias[k] = first
			continue
		}
		keyOf[j] = k
		lines = append(lines, k+" "+j)
	}
	if len(lines) > 0 {
		out, err := o.quiet(remote.Script(remote.UpgradePlan, map[string]string{"PLANS": strings.Join(lines, "\n")}))
		if err != nil {
			return nil
		}
		for k, p := range parsePlans(out, u) {
			plans[k] = p
		}
	}
	for k, first := range alias {
		if p := plans[first]; p != nil {
			plans[k] = p
		}
	}
	return plans
}

// parsePlans reads remote.UpgradePlan's output; the sizes come from u, the check that named the
// packages.
func parsePlans(out string, u *Upgrades) map[string]*Plan {
	size := map[string]int64{}
	for _, p := range u.Packages {
		size[p.Name] = p.Size
	}
	plans := map[string]*Plan{}
	var cur *Plan
	for _, l := range scanLines(out) {
		if rest, ok := strings.CutPrefix(l, "QUAY_PLAN "); ok {
			cur = nil
			if f := strings.Fields(rest); len(f) == 2 && f[1] == "0" { // apt could not: no plan
				cur = &Plan{Packages: []string{}}
				plans[f[0]] = cur
			}
			continue
		}
		if m := instRe.FindStringSubmatch(l); m != nil && cur != nil {
			cur.Packages = append(cur.Packages, m[1])
			cur.Count++
			cur.Size += size[m[1]]
		}
	}
	return plans
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
// `arduino-app-cli system update` (with --only-arduino, the update App Lab runs), or, with
// SkipDesktop, through the same apt commands without the desktop packages. Both steps run detached
// on the board, so a dropped connection does not interrupt dpkg; running it again rejoins them.
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

	if err := o.clock(); err != nil {
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
	names := u.names(opt)
	// App CLI refreshes the Arduino platform and library indexes as it lists the updates. When it
	// does not run (nothing to install, or apt installs alone), Quay does it here: on indexes older
	// than a sketch's pinned libraries, App CLI cannot download them and the sketch does not build.
	if !opt.DownloadOnly && (len(names) == 0 || opt.SkipDesktop || needPassword) {
		_ = o.step("refresh_indexes", "Refresh the Arduino platform and library indexes", func() error {
			if _, err := o.run(remote.Script(remote.RefreshIndexes, nil), nil); err != nil {
				o.warn("the Arduino indexes could not be refreshed: " + err.Error())
				return errSkip
			}
			return nil
		})
	}
	if len(names) == 0 {
		o.info("Nothing to update", nil)
		return &UpgradeResult{}, nil
	}
	list := strings.Join(names, " ")
	var fetch int64    // bytes still to download
	var installs int64 // packages that change
	pls := o.plans(u, opt, arduinoOnly(opt))
	if p := pls[opt.PlanKey()]; p != nil {
		fetch, installs = p.Size, int64(p.Count)
	} else { // apt could not say: the named packages and every new dependency
		for i := range u.Packages {
			if p := &u.Packages[i]; p.From == "" || !opt.skips(p) {
				fetch += p.Size
				installs++
			}
		}
	}

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
		cmd := "apt-get install --only-upgrade -y --allow-downgrades -d -o APT::Status-Fd=1 " + list
		if needPassword {
			return o.sudoFiltered("Download the updates", cmd+" </dev/null", filter)
		}
		_, err := o.run(remote.Script(remote.UpgradeJob, map[string]string{"CMD": "sudo -n " + cmd, "ROUND": "download"}), func(l string) bool {
			return l != "QUAY_ATTACH" && filter(l)
		})
		if err == nil {
			pr.set(Progress{Percent: 100, Done: fetch, Total: fetch, Unit: "bytes"})
		}
		if err != nil && o.ctx.Err() != nil {
			return errors.New("stopped watching; the download keeps running on the board")
		}
		if err != nil && dropped(err) {
			return ErrDropped
		}
		return err
	}); err != nil {
		return nil, err
	}
	if opt.DownloadOnly {
		o.info(fmt.Sprintf("%d packages are downloaded; install them when the board is free", installs), nil)
		return &UpgradeResult{Remaining: u.Total.Count, Downloaded: true, RebootRequired: u.RebootRequired}, nil
	}

	// "Everything" installs in two rounds when it has App Lab's packages and others. The first is
	// App Lab's own update (the arduino-* packages), whose arduino-unoq-config brings Arduino's apt
	// pins; the second works out the rest under them. In one round apt picks the rest under the old
	// rules: on the UNO Q it swaps Arduino's alsa-ucm-conf for Debian's, so the sound card goes, only
	// for the new pins to swap it back in another update.
	useAppCLI := !opt.SkipDesktop && !needPassword
	if first := u.names(arduinoOnly(opt)); !opt.ArduinoOnly && len(first) > 0 && len(first) < len(names) {
		n := int64(len(first))
		if p := pls[arduinoOnly(opt).PlanKey()]; p != nil && p.Count > 0 {
			n = int64(p.Count)
		}
		// Quay's own apt command even where arduino-app-cli installs the rest: App CLI empties the
		// apt cache when it is done, and the second round would download it all again.
		if err := o.installRound("upgrade_arduino", "Install App Lab's packages (arduino-*)", first, false, "", needPassword, n, "arduino"); err != nil {
			return nil, err
		}
		if err := s.secondRound(o, opt, useAppCLI, needPassword); err != nil {
			return nil, err
		}
	} else {
		flags := ""
		if opt.ArduinoOnly {
			flags = " --only-arduino"
		}
		if err := o.installRound("upgrade", "Install the updates", names, useAppCLI, flags, needPassword, installs, ""); err != nil {
			return nil, err
		}
	}
	if !useAppCLI { // arduino-app-cli does this as part of its own update
		if err := o.appLabExtras(); err != nil {
			return nil, err
		}
	}
	return s.upgradeResult(o, opt)
}

// appLabExtras does what App Lab's update does besides apt, after an update through Quay's own apt
// commands: arduino-app-cli system init (the Arduino platform, the libraries and build tools the
// example apps use, the app images) and system cleanup (images no longer needed; it stops a running
// app, as App Lab's update does). Without it, the first app start after the update downloads all
// that instead, unpacking the microcontroller's build tools for minutes. A failure leaves the
// update done: it is reported, and the app start downloads what is missing.
func (o *op) appLabExtras() error {
	pr := &progress{o: o, step: "applab", last: -1}
	err := o.step("applab", "Download what App Lab's update adds: the Arduino platform, the examples' libraries, the app images", func() error {
		cmd := "arduino-app-cli system init --format json-lines && arduino-app-cli system cleanup"
		_, err := o.run(remote.Script(remote.UpgradeJob, map[string]string{"CMD": cmd, "ROUND": "init"}), func(l string) bool {
			if l == "QUAY_ATTACH" {
				return false
			}
			// system init's json-lines: {"type":"progress","source":"docker","current":…,"total":…} for
			// the bytes of all images; the arduino ones count files, out of an unknown total.
			var r struct {
				Type, Source, Message string
				Current, Total        int64
			}
			if json.Unmarshal([]byte(l), &r) != nil || r.Type == "" {
				return aptNoise(l)
			}
			if r.Type == "progress" && r.Source == "docker" && r.Total > 0 {
				pr.set(Progress{Percent: 100 * float64(r.Current) / float64(r.Total), Done: r.Current, Total: r.Total, Unit: "bytes"})
			} else if r.Type != "progress" && r.Message != "" {
				o.log(r.Message)
			}
			return false
		})
		if err != nil && o.ctx.Err() != nil {
			return errors.New("stopped watching; it keeps running on the board")
		}
		if err != nil && dropped(err) {
			return ErrDropped
		}
		return err
	})
	if err != nil && !errors.Is(err, ErrDropped) && o.ctx.Err() == nil {
		o.warn("What App Lab's update adds could not be downloaded; the next app start downloads what it needs: " + err.Error())
		return nil
	}
	return err
}

// arduinoOnly is opt narrowed to App Lab's packages: the first round of an update of everything.
func arduinoOnly(opt UpgradeOptions) UpgradeOptions {
	opt.ArduinoOnly = true
	return opt
}

// secondRound installs, under the apt rules the first round brought, what an update with opt has
// left after App Lab's packages.
func (s *Session) secondRound(o *op, opt UpgradeOptions, useAppCLI, needPassword bool) error {
	out, err := o.quiet(remote.Script(remote.UpgradeList, nil))
	if err != nil {
		return err
	}
	u := ParseUpgrades(out)
	names := u.names(opt)
	if len(names) == 0 {
		return nil
	}
	n := int64(len(names))
	if p := o.plans(u, opt)[opt.PlanKey()]; p != nil && p.Count > 0 {
		n = int64(p.Count)
	}
	return o.installRound("upgrade", "Install the other updates", names, useAppCLI, "", needPassword, n, "rest")
}

// installRound installs as one step: Quay's own apt command on names (for SkipDesktop, or an image
// that wants a password for apt), or the board's arduino-app-cli system update with appCLIFlags.
// installs, the packages that change, scales the progress; round marks the job (UpgradeJob).
func (o *op) installRound(step, title string, names []string, useAppCLI bool, appCLIFlags string, needPassword bool, installs int64, round string) error {
	installs = max(installs, 1)
	// Progress from apt's status lines (pmstatus:<package>:<percent>:<text>, written at once while
	// its other output sits in a buffer when it goes to a file), or, for arduino-app-cli, which
	// streams apt's text, by its "Unpacking …" and "Setting up …" lines, two per package.
	pr := &progress{o: o, step: step, last: -1}
	var steps int64
	pm := false // apt's status lines are coming: the text lines no longer count
	count := func(l string) bool {
		if rest, ok := strings.CutPrefix(l, "pmstatus:"); ok {
			pm = true
			if f := strings.SplitN(rest, ":", 3); len(f) == 3 {
				if pc, err := strconv.ParseFloat(f[1], 64); err == nil {
					pr.set(Progress{Percent: min(99, pc), Done: min(installs, int64(pc/100*float64(installs))), Total: installs, Unit: "packages"})
				}
			}
			return false
		}
		if strings.HasPrefix(l, "dlstatus:") {
			return false
		}
		if !pm && (strings.HasPrefix(l, "Unpacking ") || strings.HasPrefix(l, "Setting up ")) {
			steps++
			pr.set(Progress{Percent: min(99, 100*float64(steps)/float64(2*installs)), Done: min(installs, steps/2), Total: installs, Unit: "packages"})
		}
		return true
	}
	// --allow-downgrades: -y alone refuses the downgrades Arduino's apt pins ask for (arduino-app-cli
	// passes it too).
	install := "apt-get install --only-upgrade -y --allow-downgrades -o APT::Status-Fd=1 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold " + strings.Join(names, " ")
	if needPassword {
		return o.step(step, title, func() error {
			_, _ = o.quiet(remote.MarkUpgrade) // what the install changes is newer than this
			return o.sudoFiltered(title, install+" </dev/null", func(l string) bool { return count(l) && aptNoise(l) })
		})
	}
	cmd := "sudo -n " + install + " && { sudo -n needrestart -r a || true; }"
	if round != "arduino" { // the cache still holds the second round's packages
		cmd += " && { sudo -n apt-get clean -y || true; }"
	}
	if useAppCLI {
		cmd = "arduino-app-cli system update --yes" + appCLIFlags
	}
	return o.step(step, title, func() error {
		completed := false
		_, err := o.run(remote.Script(remote.UpgradeJob, map[string]string{"CMD": cmd, "ROUND": round}), func(l string) bool {
			if l == "QUAY_ATTACH" {
				o.info("An update started earlier is still running on the board; following it", nil)
				return false
			}
			if strings.Contains(l, "Upgrade completed") {
				completed = true
			}
			return count(strings.TrimSpace(l)) && aptNoise(l)
		})
		if err != nil && o.ctx.Err() != nil {
			return errors.New("stopped watching; the update keeps running on the board")
		}
		if err != nil && !completed && dropped(err) {
			return ErrDropped
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
	})
}

// ErrDropped is Upgrade's error when the connection drops while it installs: the update restarts
// the services that use upgraded libraries (needrestart), sshd among them. The update goes on,
// detached, on the board; FollowUpgrade picks it up again over a new connection.
var ErrDropped = errors.New("the connection dropped while the update restarted the board's services; the update goes on on the board")

// dropped tells whether err means the connection went away under a running command: SSH's, or
// adb's when the board's adbd restarts or the cable comes out.
func dropped(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		m := e.Error()
		for _, k := range []string{"exited without exit status", "EOF", "connection reset", "broken pipe", "closed network connection",
			"error: closed", "device offline", "adb: device '", "no devices/emulators found"} {
			if strings.Contains(m, k) {
				return true
			}
		}
	}
	return false
}

// FollowUpgrade follows a system update that Upgrade started with opt, after the connection
// dropped (ErrDropped): the rest of its install if it still runs, then the result.
func (s *Session) FollowUpgrade(ctx context.Context, sink Sink, opt UpgradeOptions) (*UpgradeResult, error) {
	o := s.op(ctx, sink)
	round, _ := o.quiet("cat /tmp/quay-upgrade/round 2>/dev/null || true")
	round = strings.TrimSpace(round)
	step, title := "upgrade", "Install the updates"
	switch round {
	case "arduino":
		step, title = "upgrade_arduino", "Install App Lab's packages (arduino-*)"
	case "download":
		step, title = "download", "Download the updates"
	case "init":
		step, title = "applab", "Download what App Lab's update adds: the Arduino platform, the examples' libraries, the app images"
	}
	if err := o.step(step, title, func() error {
		_, err := o.run(remote.Script(remote.UpgradeFollow, nil), func(l string) bool {
			return !strings.HasPrefix(l, "pmstatus:") && !strings.HasPrefix(l, "dlstatus:") && aptNoise(l)
		})
		return err
	}); err != nil {
		return nil, err
	}
	// The download: the update goes on from where it was, its packages in the board's cache now.
	if round == "download" {
		return s.Upgrade(ctx, sink, opt)
	}
	// The first round of an update of everything: the rest follows, as Upgrade would have.
	if round == "arduino" && !opt.ArduinoOnly && !opt.DownloadOnly {
		if err := s.secondRound(o, opt, !opt.SkipDesktop, false); err != nil {
			return nil, err
		}
	}
	// An update through Quay's apt commands ends with what App Lab's update adds (Upgrade).
	if round != "init" && opt.SkipDesktop && !opt.DownloadOnly {
		if err := o.appLabExtras(); err != nil {
			return nil, err
		}
	}
	return s.upgradeResult(o, opt)
}

func (s *Session) upgradeResult(o *op, opt UpgradeOptions) (*UpgradeResult, error) {
	r := &UpgradeResult{}
	_ = o.step("result", "Check the result", func() error {
		out, err := o.quiet(remote.Script(remote.UpgradeList, nil))
		if err != nil {
			return err
		}
		u := ParseUpgrades(out)
		r.Remaining, r.RebootRequired = u.Total.Count, u.RebootRequired
		if pf, err := o.quiet(remote.Script(remote.PrefsChanged, nil)); err == nil {
			if files := parseRules(pf); len(files) > 0 {
				rc := &RulesChange{Files: files, Packages: []Package{}}
				for i := range u.Packages {
					if p := &u.Packages[i]; p.From != "" && !opt.skips(p) {
						rc.Packages = append(rc.Packages, *p)
					}
				}
				if len(rc.Packages) > 0 {
					r.Rules = rc
					o.info(fmt.Sprintf("The update changed apt's version rules (%s); under them %d more packages change, which Quay installs only when asked", files[0].Path, len(rc.Packages)), nil)
				}
			}
		}
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
