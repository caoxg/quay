package core

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/relay"
	"github.com/caoxg/quay/internal/remote"
	"github.com/caoxg/quay/internal/version"
)

// DefaultMirror is the PyPI mirror `mirror cn` uses unless told otherwise.
const DefaultMirror = "https://mirrors.tuna.tsinghua.edu.cn/pypi/web/simple"

// DefaultDebianMirror is the Debian mirror `mirror cn` uses unless told otherwise (its security
// updates are expected at the same URL + "-security", as on every major mirror).
const DefaultDebianMirror = "https://mirrors.tuna.tsinghua.edu.cn/debian"

// MirrorOptions choose the mirrors; PyPI and Debian both empty restores the original configuration.
type MirrorOptions struct {
	PyPI   string `json:"pypi"`   // pip and uv index
	Debian string `json:"debian"` // apt's Debian packages
	// PyPIFallback becomes pip's extra index: pip carries on with it when PyPI fails.
	PyPIFallback string `json:"pypiFallback,omitempty"`
	// DebianFallback are the mirrors apt moves to, in order, when Debian fails; the official
	// server is always the last resort.
	DebianFallback []string `json:"debianFallback,omitempty"`
}

// Prepare applies (or reverts) an app's one-time system setup. It needs sudo.
func (s *Session) Prepare(ctx context.Context, sink Sink, key string, revert, force bool) error {
	o := s.op(ctx, sink)
	_, a, err := s.lookup(ctx, key)
	if err != nil {
		return err
	}
	if a.Profile == nil || a.Profile.System == nil {
		return fmt.Errorf("%s declares no system setup", a.Name)
	}
	sys := a.Profile.System
	body := sys.Apply
	verb := "Apply"
	if revert {
		if strings.TrimSpace(sys.Revert) == "" {
			return fmt.Errorf("%s cannot revert its system setup", a.Name)
		}
		body, verb = sys.Revert, "Revert"
	}
	wasRunning := active(a.Status)
	if sys.StopApp && wasRunning {
		if err := o.step("safety", "Check it is safe to stop "+a.Name, func() error { return o.guardStop(a, force) }); err != nil {
			return err
		}
		if err := o.step("stop", "Stop "+a.Name, func() error { return o.stopApp(a.Path) }); err != nil {
			return err
		}
	}
	if err := o.step("system", verb+" system setup: "+firstSentence(sys.Description), func() error {
		return o.sudo(verb+" system setup", remote.SystemHook, map[string]string{"APPDIR": a.Path, "RUN": body})
	}); err != nil {
		return err
	}
	if strings.TrimSpace(sys.Check) != "" {
		_ = o.step("verify", "Verify", func() error {
			ok, err := s.systemReady(ctx, a.Folder, a.Profile)
			if err != nil {
				return err
			}
			if ok == revert {
				return errors.New("the check does not confirm the change")
			}
			return nil
		})
	}
	if sys.StopApp && wasRunning && !revert {
		return o.step("start", "Start "+a.Name+" again", func() error { return o.startApp(a.Path, a.Profile) })
	}
	return nil
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i]
	}
	return strings.TrimSuffix(s, ".")
}

// Mirror points the board's pip and uv at a PyPI mirror (index != "") or restores them ("").
func (s *Session) Mirror(ctx context.Context, sink Sink, m MirrorOptions) error {
	o := s.op(ctx, sink)
	if m.PyPI == "" && m.Debian == "" {
		return o.step("mirror", "Restore the original pip, uv and apt configuration", func() error {
			return o.sudo("Mirror off", remote.MirrorOff, nil)
		})
	}
	var what []string
	for _, u := range []struct{ url, name string }{{m.PyPI, "pip and uv"}, {m.Debian, "Debian packages"}} {
		if u.url != "" {
			what = append(what, u.name)
		}
	}
	for _, u := range append([]string{m.PyPI, m.Debian, m.PyPIFallback}, m.DebianFallback...) {
		if u == "" {
			continue
		}
		if (!strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://")) || strings.ContainsAny(u, " \t\n'\"") {
			return fmt.Errorf("mirror must be an http(s) URL: %q", u)
		}
	}
	fallback := ""
	if m.Debian != "" {
		fallback = strings.Join(m.DebianFallback, " ")
	}
	pypiFallback := ""
	if m.PyPI != "" && m.PyPIFallback != m.PyPI {
		pypiFallback = m.PyPIFallback
	}
	return o.step("mirror", "Use mirrors for "+strings.Join(what, " and "), func() error {
		return o.sudo("Mirror on", remote.MirrorOn, map[string]string{"INDEX": m.PyPI, "INDEX_FALLBACK": pypiFallback, "DEBIAN": m.Debian, "DEBIAN_FALLBACK": fallback})
	})
}

// Relay installs (on) or removes the board's apt hook that downloads through this computer.
func (s *Session) Relay(ctx context.Context, sink Sink, on bool) error {
	o := s.op(ctx, sink)
	if !on {
		return o.step("relay", "Download directly again", func() error {
			return o.sudo("Relay off", remote.RelayOff, nil)
		})
	}
	return o.step("relay", "Download through this computer while Quay is connected", func() error {
		return o.sudo("Relay on", remote.RelayOn, map[string]string{"PORT": strconv.Itoa(relay.Port), "PROBE": relay.ProbeHost})
	})
}

// BackupMeta is stored inside each backup.
type BackupMeta struct {
	Folder   string    `json:"folder"`
	App      string    `json:"app"`
	Serial   string    `json:"serial"`
	Hostname string    `json:"hostname"`
	Commit   string    `json:"commit,omitempty"`
	Created  time.Time `json:"created"`
	Quay     string    `json:"quay"`
}

// Backup copies an app's data/ to dir; returns "" when data/ is empty.
func (s *Session) Backup(ctx context.Context, sink Sink, key, dir string) (string, error) {
	o := s.op(ctx, sink)
	st, a, err := s.lookup(ctx, key)
	if err != nil {
		return "", err
	}
	var path string
	err = o.step("backup", "Back up "+a.Name+" data/", func() error {
		if len(a.Data) == 0 {
			o.info("data/ is empty: nothing to back up", nil)
			return errSkip
		}
		var err error
		path, err = o.backup(st, a, dir)
		return err
	})
	return path, err
}

func (o *op) backup(st *Status, a *App, dir string) (string, error) {
	meta := BackupMeta{Folder: a.Folder, App: a.Name, Serial: st.Serial, Hostname: st.Hostname, Created: time.Now().UTC(), Quay: version.Version}
	if a.Deploy != nil {
		meta.Commit = a.Deploy.Commit
	}
	mj, _ := json.Marshal(meta)
	tmp := "/tmp/quay-backup-" + randHex(6) + ".tgz"
	out, err := o.quiet(remote.Script(remote.Backup, map[string]string{"FOLDER": a.Folder, "OUT": tmp, "META": string(mj)}))
	if err != nil {
		return "", err
	}
	if strings.Contains(out, "QUAY_EMPTY") {
		return "", errSkip
	}
	defer func() { _ = o.s.T.Exec(context.Background(), "rm -f "+board.Quote(tmp), nil, nil, nil) }()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	serial := meta.Serial
	if serial == "" {
		serial = st.Hostname
	}
	name := fmt.Sprintf("%s-%s-%s.tgz", a.Folder, safeName(serial), time.Now().Format("20060102-150405"))
	local := filepath.Join(dir, name)
	f, err := os.Create(local)
	if err != nil {
		return "", err
	}
	if err := o.s.T.Download(o.ctx, tmp, f); err != nil {
		f.Close()
		os.Remove(local)
		return "", err
	}
	f.Close()
	if _, err := ReadBackupMeta(local); err != nil {
		os.Remove(local)
		return "", fmt.Errorf("backup transfer was corrupted: %w", err)
	}
	o.info("saved "+local, map[string]string{"path": local})
	return local, nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// ReadBackupMeta checks a backup file and returns its metadata.
func ReadBackupMeta(path string) (*BackupMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("not a Quay backup: %w", err)
	}
	tr := tar.NewReader(gz)
	var meta *BackupMeta
	hasData := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not a Quay backup: %w", err)
		}
		switch {
		case h.Name == "quay-backup.json":
			var m BackupMeta
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return nil, fmt.Errorf("backup metadata: %w", err)
			}
			meta = &m
		case h.Name == "data" || strings.HasPrefix(h.Name, "data/"):
			hasData = true
		}
	}
	if !hasData {
		return nil, errors.New("not a Quay backup (no data/ folder)")
	}
	if meta == nil {
		meta = &BackupMeta{}
	}
	return meta, nil
}

// Restore puts a backup's data/ into an app. The current data/ is kept as a restore point.
func (s *Session) Restore(ctx context.Context, sink Sink, key, file string, force bool) error {
	o := s.op(ctx, sink)
	st, a, err := s.lookup(ctx, key)
	if err != nil {
		return err
	}
	meta, err := ReadBackupMeta(file)
	if err != nil {
		return err
	}
	if err := o.step("check", "Check the backup", func() error {
		if !force && meta.Serial != "" && st.Serial != "" && meta.Serial != st.Serial {
			return fmt.Errorf("this backup is from board %s (%s), not this one (%s); use --force to restore anyway", meta.Hostname, meta.Serial, st.Serial)
		}
		if meta.Folder != "" && meta.Folder != a.Folder {
			o.warn(fmt.Sprintf("backup was made from %s, restoring into %s", meta.Folder, a.Folder))
		}
		return nil
	}); err != nil {
		return err
	}
	wasRunning := active(a.Status)
	if wasRunning {
		if err := o.step("safety", "Check it is safe to stop "+a.Name, func() error { return o.guardStop(a, force) }); err != nil {
			return err
		}
		if err := o.step("stop", "Stop "+a.Name, func() error { return o.stopApp(a.Path) }); err != nil {
			return err
		}
	}
	pol := a.Profile != nil && a.Profile.Policies != nil
	if pol && len(a.Policies) > 0 {
		// The backup brings its own data/quay-policies; put the stock models back first, so the
		// policies it lists are applied over the stock ones and its stock copies stay honest.
		if err := o.step("stock", "Put the stock models back", func() error {
			ids := make([]string, 0, len(a.Policies))
			for id := range a.Policies {
				ids = append(ids, id)
			}
			_, err := s.policyScript(ctx, a, a.Profile.Policies, map[string]any{"op": "restore", "restore": ids})
			return err
		}); err != nil {
			return err
		}
	}
	tarball := "/tmp/quay-restore-" + randHex(6) + ".tgz"
	if err := o.step("restore", "Restore data/", func() error {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := s.T.Upload(ctx, f, tarball); err != nil {
			return err
		}
		_, err = o.run(remote.Script(remote.Restore, map[string]string{"FOLDER": a.Folder, "TARBALL": tarball}), nil)
		return err
	}); err != nil {
		return err
	}
	if pol {
		if err := o.reapplyPolicies(a, a.Profile); err != nil {
			o.warn("the backup's policies could not be put back, so the app uses its stock models: " + err.Error())
		}
	}
	if wasRunning {
		return o.step("start", "Start "+a.Name+" again", func() error { return o.startApp(a.Path, a.Profile) })
	}
	return nil
}

// EraseOptions controls Erase.
type EraseOptions struct {
	Backup       bool
	BackupDir    string
	Force        bool
	RevertSystem bool // undo the app's system setup first (needs sudo)
}

// Erase removes an app from the board: its containers and caches (App CLI destroy), the boot
// default if it is one, the app's own uninstall hook, then the folder. data/ is backed up first.
func (s *Session) Erase(ctx context.Context, sink Sink, key string, opt EraseOptions) (backup string, err error) {
	o := s.op(ctx, sink)
	st, a, err := s.lookup(ctx, key)
	if err != nil {
		return "", err
	}
	if err := ValidFolder(a.Folder); err != nil {
		return "", err
	}
	if err := o.step("safety", "Check it is safe to stop "+a.Name, func() error {
		if !active(a.Status) {
			return errSkip
		}
		return o.guardStop(a, opt.Force)
	}); err != nil {
		return "", err
	}
	if err := o.step("backup", "Back up data/", func() error {
		if !opt.Backup || len(a.Data) == 0 {
			return errSkip
		}
		var err error
		backup, err = o.backup(st, a, opt.BackupDir)
		return err
	}); err != nil {
		return "", err
	}
	_ = o.step("destroy", "Stop it and remove its containers and caches", func() error {
		if a.Status == "uninitialized" || a.Status == "" {
			return errSkip
		}
		if _, err := o.run("arduino-app-cli app destroy "+board.Quote(a.Path), nil); err != nil {
			o.warn("app destroy: " + err.Error() + " (continuing)")
		}
		return nil
	})
	if err := o.step("autostart", "Clear the boot default", func() error {
		if !a.Default {
			return errSkip
		}
		_, err := o.run("arduino-app-cli properties set default none", nil)
		return err
	}); err != nil {
		return backup, err
	}
	if err := o.step("uninstall", "Run the app's uninstall hook", func() error {
		if a.Profile == nil || a.Profile.Uninstall == nil || strings.TrimSpace(a.Profile.Uninstall.Run) == "" {
			return errSkip
		}
		_, err := o.run(remote.Script(remote.Uninstall, map[string]string{"FOLDER": a.Folder, "RUN": a.Profile.Uninstall.Run}), nil)
		return err
	}); err != nil {
		return backup, err
	}
	applied := a.Profile != nil && a.Profile.System != nil && a.SystemReady != nil && *a.SystemReady
	if err := o.step("system", "Revert the app's system setup", func() error {
		if !opt.RevertSystem || !applied || strings.TrimSpace(a.Profile.System.Revert) == "" {
			return errSkip
		}
		return o.sudo("Revert system setup", remote.SystemHook, map[string]string{"APPDIR": a.Path, "RUN": a.Profile.System.Revert})
	}); err != nil {
		return backup, err
	}
	err = o.step("remove", "Delete ~/ArduinoApps/"+a.Folder, func() error {
		_, err := o.run(remote.Script(remote.RemoveFolder, map[string]string{"FOLDER": a.Folder}), nil)
		return err
	})
	if err == nil && applied && !opt.RevertSystem {
		o.warn(a.Profile.Name + "'s system setup is still applied (" + firstSentence(a.Profile.System.Description) + "). Other apps may need the stock settings.")
	}
	return backup, err
}
