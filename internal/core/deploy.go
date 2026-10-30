package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/caoxg/quay/internal/profile"
	"github.com/caoxg/quay/internal/remote"
	"github.com/caoxg/quay/internal/source"
	"github.com/caoxg/quay/internal/version"
)

// FetchOptions selects what to deploy.
type FetchOptions struct {
	Repo   string // git URL / owner/repo; empty when Dir is set
	Ref    string // branch, tag or commit; empty = default branch
	Subdir string // app folder inside the repo
	Dir    string // deploy a local folder instead
}

// Fetch prepares a bundle on this computer (no board needed).
func Fetch(ctx context.Context, sink Sink, cacheDir string, f FetchOptions) (*source.Bundle, error) {
	o := (&Session{}).op(ctx, sink)
	var b *source.Bundle
	what := f.Repo
	if f.Ref != "" {
		what += " @ " + f.Ref
	}
	if f.Dir != "" {
		what = f.Dir
	}
	err := o.step("fetch", "Get "+what, func() error {
		var err error
		if f.Dir != "" {
			b, err = source.FromDir(f.Dir)
		} else {
			lw := &lines{o: o, filter: func(l string) bool { return !strings.Contains(l, "\r") }}
			b, err = source.Fetch(ctx, cacheDir, f.Repo, f.Ref, f.Subdir, lw)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	short := b.Commit
	if len(short) > 7 {
		short = short[:7]
	}
	o.info(fmt.Sprintf("%s · %s %s", b.App.Name, b.Ref, short), b)
	return b, nil
}

// DeployOptions controls Deploy.
type DeployOptions struct {
	Folder string // install folder under ~/ArduinoApps (default: profile/repo name)
	Start  bool   // start after installing
	Force  bool   // ignore the app's safety probe
	Backup bool   // back up data/ first (when not empty)
	// Setup applies the profile's one-time system setup (sudo) before starting, when its check
	// says it is not done (or it has no check). The app is stopped at that point, so it costs no
	// extra stop/start.
	Setup bool
	// BackupDir is where automatic backups go.
	BackupDir string
	// Expect (Update only) is the commit the user agreed to install: if the branch has moved on
	// since, the update stops rather than install commits nobody looked at.
	Expect string
}

// DeployResult summarises a deploy.
type DeployResult struct {
	Folder     string           `json:"folder"`
	Path       string           `json:"path"`
	Commit     string           `json:"commit"`
	Previous   string           `json:"previous,omitempty"`
	Profile    *profile.Profile `json:"profile,omitempty"`
	Backup     string           `json:"backup,omitempty"`
	Rebuilt    bool             `json:"rebuilt"`
	NeedsSetup bool             `json:"needsSetup"`
}

// Deploy installs or updates an app from a bundle, keeping its data/.
func (s *Session) Deploy(ctx context.Context, sink Sink, b *source.Bundle, opt DeployOptions) (*DeployResult, error) {
	o := s.op(ctx, sink)
	prof, err := profile.Lookup(b.QuayYAML, b.Repo, b.App.Name)
	if err != nil {
		return nil, err
	}
	folder := opt.Folder
	if folder == "" && prof != nil && prof.Folder != "" {
		folder = prof.Folder
	}
	if folder == "" {
		folder = b.Folder
	}
	if err := ValidFolder(folder); err != nil {
		return nil, err
	}
	if err := o.clock(); err != nil { // the app's first start downloads its packages
		return nil, err
	}
	res := &DeployResult{Folder: folder, Commit: b.Commit, Profile: prof}

	var st *Status
	var cur *App
	if err := o.step("board", "Check the board", func() error {
		var err error
		st, err = s.Status(ctx)
		if err != nil {
			return err
		}
		if !st.AppCLI {
			return fmt.Errorf("arduino-app-cli not found on %s — Quay needs an Arduino UNO Q with the stock image", st.Hostname)
		}
		if !st.IsUnoQ {
			o.warn("this board does not identify as an Arduino UNO Q; continuing")
		}
		res.Path = st.Home + "/ArduinoApps/" + folder
		for i := range st.Apps {
			if st.Apps[i].Folder == folder {
				cur = &st.Apps[i]
			}
		}
		if cur != nil && cur.Deploy != nil {
			res.Previous = cur.Deploy.Commit
			if cur.Deploy.Repo != "" && cur.Deploy.Repo != b.Repo && profile.NormalizeRepo(cur.Deploy.Repo) != profile.NormalizeRepo(b.Repo) {
				o.warn(fmt.Sprintf("%s was installed from %s; replacing it with %s", folder, cur.Deploy.Repo, b.Repo))
			}
		} else if cur != nil {
			o.warn(folder + " exists but was not installed by Quay; its code will be replaced, data/ is kept")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if cur != nil {
		cur.Profile = prof
		if err := o.step("safety", "Check it is safe to stop "+cur.Name, func() error {
			if !active(cur.Status) {
				return errSkip
			}
			return o.guardStop(cur, opt.Force)
		}); err != nil {
			return nil, err
		}
		if err := o.step("backup", "Back up data/", func() error {
			if !opt.Backup || len(cur.Data) == 0 {
				return errSkip
			}
			p, err := o.backup(st, cur, opt.BackupDir)
			res.Backup = p
			return err
		}); err != nil {
			return nil, err
		}
		if err := o.step("stop", "Stop "+cur.Name, func() error {
			if !active(cur.Status) {
				return errSkip
			}
			return o.stopApp(cur.Path)
		}); err != nil {
			return nil, err
		}
	}

	tarball := "/tmp/quay-" + randHex(6) + ".tar"
	if err := o.step("upload", fmt.Sprintf("Upload %d files (%s)", b.Files, human(len(b.Tar))), func() error {
		return s.T.Upload(ctx, bytes.NewReader(b.Tar), tarball)
	}); err != nil {
		return nil, err
	}
	meta := Deployment{Repo: b.Repo, Ref: b.Ref, Commit: b.Commit, Subject: b.Subject, CommitDate: b.Date, Subdir: b.Subdir,
		Dirty: b.Dirty, DeployedAt: time.Now().UTC(), Quay: version.Version}
	if prof != nil {
		meta.Profile = prof.ID
	}
	mj, _ := json.Marshal(meta)
	if err := o.step("install", "Install into ~/ArduinoApps/"+folder+" (data/ kept)", func() error {
		_, err := o.run(remote.Script(remote.Replace, map[string]string{"FOLDER": folder, "TARBALL": tarball, "META": string(mj)}), nil)
		return err
	}); err != nil {
		return nil, err
	}
	if err := o.step("indexes", "Check the board's library index has the sketch's pinned versions", func() error {
		out, err := o.run(remote.Script(remote.Indexes, map[string]string{"FOLDER": folder}), func(l string) bool { return l != "QUAY_NOSKETCH" })
		if err == nil && strings.Contains(out, "QUAY_NOSKETCH") {
			return errSkip
		}
		return err
	}); err != nil {
		return nil, err
	}
	if err := o.step("before_start", "Run the app's preparation hook", func() error {
		if prof == nil || prof.BeforeStart == nil || strings.TrimSpace(prof.BeforeStart.Run) == "" {
			return errSkip
		}
		resolve := "0"
		if prof.BeforeStart.ResolveOnFailure {
			resolve = "1"
		}
		out, err := o.run(remote.Script(remote.BeforeStart, map[string]string{
			"FOLDER": folder, "RUN": prof.BeforeStart.Run, "RESOLVE": resolve,
			"GLOBS": strings.Join(prof.BeforeStart.RebuildWhenChanged, " "),
		}), func(l string) bool {
			if strings.HasPrefix(l, "first build:") {
				o.info("First build of this sketch on the board: App CLI downloads the microcontroller's build tools (about 100 MB), builds and flashes the firmware, then builds it again after the hook. This takes 10 to 20 minutes.", map[string]string{"note": "firstBuild"})
			}
			return l != "QUAY_REBUILD"
		})
		if strings.Contains(out, "QUAY_REBUILD") {
			res.Rebuilt = true
			o.info("hook changed build inputs: the sketch will be rebuilt", nil)
		}
		return err
	}); err != nil {
		return nil, err
	}
	if cur != nil {
		if err := o.reapplyPolicies(&App{Folder: folder, Path: res.Path, Name: cur.Name}, prof); err != nil {
			o.warn("the installed policies could not be put back, so the app starts with its stock models: " + err.Error())
		}
	}
	if prof != nil && prof.System != nil {
		ready := false
		if prof.System.Check != "" {
			ok, err := s.systemReady(ctx, folder, prof)
			ready = err == nil && ok
		}
		switch {
		case ready:
		case opt.Setup:
			if err := o.step("system", "Apply system setup: "+firstSentence(prof.System.Description), func() error {
				return o.sudo("Apply system setup", remote.SystemHook, map[string]string{"APPDIR": res.Path, "RUN": prof.System.Apply})
			}); err != nil {
				return res, err
			}
		case prof.System.Check != "":
			res.NeedsSetup = true
			o.warn("one-time system setup for " + prof.Name + " is not done yet: run Prepare (it needs sudo) before the app can work")
		}
	}
	if opt.Start {
		if err := o.step("start", "Start the app", func() error { return o.startApp(res.Path, prof) }); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Update redeploys an installed app from the repository recorded at install time.
func (s *Session) Update(ctx context.Context, sink Sink, cacheDir, key, ref string, opt DeployOptions) (*DeployResult, error) {
	_, a, err := s.lookup(ctx, key)
	if err != nil {
		return nil, err
	}
	if a.Deploy == nil || a.Deploy.Repo == "" {
		return nil, fmt.Errorf("%s was not installed by Quay; deploy it from its repository first", a.Folder)
	}
	if strings.HasPrefix(a.Deploy.Repo, "local:") {
		return nil, fmt.Errorf("%s was deployed from a local folder (%s); deploy that folder again", a.Folder, strings.TrimPrefix(a.Deploy.Repo, "local:"))
	}
	if ref == "" {
		ref = a.Deploy.Ref
		if LooksLikeCommit(ref) { // pinned to a commit: follow the default branch
			ref = ""
		}
	}
	b, err := Fetch(ctx, sink, cacheDir, FetchOptions{Repo: a.Deploy.Repo, Ref: ref, Subdir: a.Deploy.Subdir})
	if err != nil {
		return nil, err
	}
	if opt.Expect != "" && b.Commit != opt.Expect {
		return nil, fmt.Errorf("%s has moved on to %s since you looked (you agreed to %s); look at what it changes again", b.Ref, shortHash(b.Commit), shortHash(opt.Expect))
	}
	if b.Commit == a.Deploy.Commit && !opt.Force {
		o := s.op(ctx, sink)
		o.info("already up to date", nil)
		return &DeployResult{Folder: a.Folder, Path: a.Path, Commit: b.Commit, Previous: b.Commit, Profile: a.Profile}, nil
	}
	opt.Folder = a.Folder
	opt.Start = opt.Start || active(a.Status) // an app that was running comes back after the update
	return s.Deploy(ctx, sink, b, opt)
}

func shortHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

// LooksLikeCommit tells a commit hash from a branch or tag name.
func LooksLikeCommit(ref string) bool {
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for _, r := range ref {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func (s *Session) systemReady(ctx context.Context, folder string, p *profile.Profile) (bool, error) {
	out, err := (&op{ctx: ctx, s: s, sink: func(Event) {}}).quiet(remote.Script(remote.CheckAll, map[string]string{"CHECKS": folder + "\t" + strings.ReplaceAll(p.System.Check, "\n", " ; ")}))
	if err != nil {
		return false, err
	}
	return strings.Contains(out, folder+"\t1"), nil
}

func human(n int) string {
	switch {
	case n > 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

var _ = io.Discard
