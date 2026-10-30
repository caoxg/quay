// Package selfupdate keeps Quay itself up to date from its GitHub releases, separately from the
// profiles (internal/hub). A release is trusted only when its SHA256SUMS carries a valid Ed25519
// signature by a key whose public half is built in (signed with build/sign), and the downloaded
// file matches its line in SHA256SUMS. Nothing is installed unless the user asks.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/caoxg/quay/internal/version"
)

// PublicKeys verify release signatures (base64 Ed25519). More than one lets a key be replaced:
// sign with the new key while Quays that only know the old one are still around.
var PublicKeys = []string{
	"nXEWN1vJMZ1DDuwNJOxAwFeTslSgm2bGzSGUAk7xIJ8=", // 2026-10-26, ~/.config/quay/release-signing.key on the maintainer's Mac
}

// Repo is where releases are published.
var Repo = "caoxg/quay"

// API and Web are GitHub's endpoints (replaced in tests; QUAY_UPDATE_SERVER points both at a test
// server — what it serves must still be signed by the release key).
var (
	API = "https://api.github.com"
	Web = "https://github.com"
)

func init() {
	if s := os.Getenv("QUAY_UPDATE_SERVER"); s != "" {
		API, Web = strings.TrimRight(s, "/"), strings.TrimRight(s, "/")
	}
}

// HasGUI is set by the desktop app's package: this binary is the app, not the command line only.
var HasGUI bool

// Release is a published release.
type Release struct {
	Version   string           `json:"version"` // the tag, e.g. v0.2.0
	Name      string           `json:"name"`
	Notes     string           `json:"notes"`
	URL       string           `json:"url"`
	Published time.Time        `json:"published"`
	Assets    map[string]Asset `json:"assets,omitempty"`
}

// Asset is a file of a release.
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"` // 0 when unknown
	URL  string `json:"url"`
}

// Updater checks and installs. Client defaults to one that goes through Proxy.
type Updater struct {
	Client *http.Client
	Proxy  func() (*url.URL, error) // the proxy to download through (nil: direct)
	Exe    string                   // the running program (default os.Executable)
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if u.Proxy != nil {
		tr.Proxy = func(r *http.Request) (*url.URL, error) {
			if h := r.URL.Hostname(); h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.") {
				return nil, nil // a local server (tests): never through the proxy
			}
			return u.Proxy()
		}
	}
	return &http.Client{Transport: tr}
}

func (u *Updater) get(ctx context.Context, rawURL string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Quay/"+version.Version)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d", rawURL, resp.StatusCode)
	}
	return resp, nil
}

// Latest asks GitHub for the newest published release. When the API cannot be reached (it is
// often slow in mainland China), the release page's redirect still names the tag; the notes and
// sizes are then unknown.
func (u *Updater) Latest(ctx context.Context) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := u.get(ctx, API+"/repos/"+Repo+"/releases/latest", "application/vnd.github+json")
	if err == nil {
		defer resp.Body.Close()
		var r struct {
			Tag       string    `json:"tag_name"`
			Name      string    `json:"name"`
			Body      string    `json:"body"`
			HTMLURL   string    `json:"html_url"`
			Published time.Time `json:"published_at"`
			Assets    []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
				URL  string `json:"browser_download_url"`
			} `json:"assets"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
			return nil, err
		}
		rel := &Release{Version: r.Tag, Name: r.Name, Notes: r.Body, URL: r.HTMLURL, Published: r.Published, Assets: map[string]Asset{}}
		for _, a := range r.Assets {
			rel.Assets[a.Name] = Asset{Name: a.Name, Size: a.Size, URL: a.URL}
		}
		return rel, nil
	}
	tag, err2 := u.latestTag(ctx)
	if err2 != nil {
		return nil, fmt.Errorf("%v; %v", err, err2)
	}
	return &Release{Version: tag, URL: Web + "/" + Repo + "/releases/tag/" + tag}, nil
}

func (u *Updater) latestTag(ctx context.Context) (string, error) {
	c := *u.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Web+"/"+Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("no latest release (HTTP %d)", resp.StatusCode)
	}
	return loc[i+5:], nil
}

// Size is a file's size, when the release says (0 otherwise).
func (r *Release) Size(name string) int64 { return r.Assets[name].Size }

// asset returns a release file by name, from the API's list or, without one, at its usual URL.
func (r *Release) asset(name string) Asset {
	if a, ok := r.Assets[name]; ok {
		return a
	}
	return Asset{Name: name, URL: Web + "/" + Repo + "/releases/download/" + r.Version + "/" + name}
}

// versionNow is the running version (a variable for tests).
var versionNow = func() string { return version.Version }

// Newer reports whether release version v is newer than the running one (cur). A development
// build ("dev") is never offered updates.
func Newer(v, cur string) bool {
	a, ok1 := semver(v)
	b, ok2 := semver(cur)
	if !ok1 || !ok2 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// gitDescribe is what `git describe --dirty` adds to a build made after a tag (3-gd887a3a,
// 3-gd887a3a-dirty) or from a changed checkout of it (dirty).
var gitDescribe = regexp.MustCompile(`^([0-9]+-g[0-9a-f]+)?(-?dirty)?$`)

// semver reads vMAJOR.MINOR.PATCH; a pre-release (v1.2.3-rc1) counts below the release, and a
// development build made after it (v1.2.3-3-gd887a3a) above it.
func semver(s string) ([4]int, bool) {
	var out [4]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	main, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(main, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	out[3] = 1
	if hasPre && pre != "" {
		out[3] = 0
		if gitDescribe.MatchString(pre) {
			out[3] = 2
		}
	}
	return out, true
}

// ---------------------------------------------------------------- where Quay is installed

// Kinds of installation.
const (
	KindMacApp = "mac-app" // Quay.app: replaced with the universal .zip
	KindWinZip = "win-zip" // a folder with Quay.exe and bin\quay.exe: replaced with the Windows .zip
	KindTarGz  = "tar-gz"  // the Linux app from the .tar.gz: the binary is replaced
	KindBinary = "binary"  // a command-line binary (quay-cli-…): replaced
	KindDeb    = "deb"     // /usr/bin/quay from the .deb: the .deb is downloaded for apt
)

// Target is the installation the running program belongs to.
type Target struct {
	Kind string `json:"kind"`
	Path string `json:"path"` // what gets replaced: the .app, the folder, or the binary
}

func (u *Updater) exe() (string, error) {
	if u.Exe != "" {
		return u.Exe, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// Where finds the installation the running program belongs to.
func (u *Updater) Where() (Target, error) {
	exe, err := u.exe()
	if err != nil {
		return Target{}, err
	}
	switch runtime.GOOS {
	case "darwin":
		if i := strings.Index(exe, ".app/Contents/MacOS/"); i > 0 {
			return Target{Kind: KindMacApp, Path: exe[:i+4]}, nil
		}
	case "windows":
		dir := filepath.Dir(exe)
		if strings.EqualFold(filepath.Base(exe), "Quay.exe") {
			return Target{Kind: KindWinZip, Path: dir}, nil
		}
		if strings.EqualFold(filepath.Base(dir), "bin") && exists(filepath.Join(filepath.Dir(dir), "Quay.exe")) {
			return Target{Kind: KindWinZip, Path: filepath.Dir(dir)}, nil
		}
	case "linux":
		if exe == "/usr/bin/quay" {
			return Target{Kind: KindDeb, Path: exe}, nil
		}
		if HasGUI {
			return Target{Kind: KindTarGz, Path: exe}, nil
		}
	}
	return Target{Kind: KindBinary, Path: exe}, nil
}

// AssetName is the release file for an installation.
func AssetName(kind, tag string) string {
	arch := runtime.GOARCH
	switch kind {
	case KindMacApp:
		return "Quay-" + tag + "-macos-universal.zip"
	case KindWinZip:
		return "Quay-" + tag + "-windows-" + arch + ".zip"
	case KindTarGz:
		return "quay-" + tag + "-linux-" + arch + ".tar.gz"
	case KindDeb:
		return "quay_" + strings.TrimPrefix(tag, "v") + "_" + arch + ".deb"
	}
	name := "quay-cli-" + tag + "-" + runtime.GOOS + "-" + arch
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// ---------------------------------------------------------------- verifying

// Verify checks a base64 Ed25519 signature over data against the built-in public keys.
func Verify(data, sig []byte) error {
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(s) != ed25519.SignatureSize {
		return errors.New("malformed signature")
	}
	for _, k := range PublicKeys {
		pub, err := base64.StdEncoding.DecodeString(k)
		if err == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, data, s) {
			return nil
		}
	}
	return errors.New("the signature does not match any key Quay trusts")
}

// sums parses SHA256SUMS ("<hex>  <name>" lines).
func sums(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && len(f[0]) == 64 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out
}

func (u *Updater) small(ctx context.Context, a Asset) ([]byte, error) {
	resp, err := u.get(ctx, a.URL, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ---------------------------------------------------------------- installing

// Result says what Install did: the new program is in place (Relaunch: start it), or the user has a
// last step to take (Manual, for the .deb).
type Result struct {
	Version  string `json:"version"`
	Target   Target `json:"target"`
	Relaunch bool   `json:"relaunch"`
	Manual   string `json:"manual,omitempty"`
	File     string `json:"file,omitempty"` // the downloaded .deb
}

// Install downloads the release's file for this installation, verifies it and puts it in place.
// progress reports the download (total 0 when unknown).
func (u *Updater) Install(ctx context.Context, rel *Release, progress func(done, total int64)) (*Result, error) {
	t, err := u.Where()
	if err != nil {
		return nil, err
	}
	name := AssetName(t.Kind, rel.Version)
	sumsFile, err := u.small(ctx, rel.asset("SHA256SUMS"))
	if err != nil {
		return nil, fmt.Errorf("SHA256SUMS: %w", err)
	}
	sig, err := u.small(ctx, rel.asset("SHA256SUMS.sig"))
	if err != nil {
		return nil, fmt.Errorf("SHA256SUMS.sig (an unsigned release is never installed): %w", err)
	}
	if err := Verify(sumsFile, sig); err != nil {
		return nil, fmt.Errorf("release %s: %w", rel.Version, err)
	}
	want, ok := sums(sumsFile)[name]
	if !ok {
		return nil, fmt.Errorf("release %s has no %s", rel.Version, name)
	}

	dir := filepath.Dir(t.Path) // stage next to the target, so the final rename stays on one disk
	if t.Kind == KindDeb {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "Downloads")
		_ = os.MkdirAll(dir, 0o755)
	}
	stage, err := os.MkdirTemp(dir, ".quay-update-")
	if err != nil {
		return nil, fmt.Errorf("cannot write next to %s: %w", t.Path, err)
	}
	defer os.RemoveAll(stage)
	file := filepath.Join(stage, name)
	if err := u.download(ctx, rel.asset(name), file, want, progress); err != nil {
		return nil, err
	}
	res := &Result{Version: rel.Version, Target: t}
	switch t.Kind {
	case KindDeb:
		final := filepath.Join(dir, name)
		if err := os.Rename(file, final); err != nil {
			return nil, err
		}
		res.File, res.Manual = final, "sudo apt install "+final
		return res, nil
	case KindMacApp:
		err = installMacApp(ctx, file, stage, t.Path, rel.Version)
	case KindWinZip:
		err = installWinZip(file, stage, t.Path)
	case KindTarGz:
		var bin string
		if bin, err = untarBinary(file, stage); err == nil {
			err = replaceFile(bin, t.Path)
		}
	default:
		err = replaceFile(file, t.Path)
	}
	if err != nil {
		return nil, err
	}
	res.Relaunch = true
	return res, nil
}

func (u *Updater) download(ctx context.Context, a Asset, dst, want string, progress func(done, total int64)) error {
	resp, err := u.get(ctx, a.URL, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	total := resp.ContentLength
	if total <= 0 {
		total = a.Size
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	h := sha256.New()
	var done int64
	buf := make([]byte, 256<<10)
	last := time.Time{}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			h.Write(buf[:n])
			done += int64(n)
			if done > 1<<30 {
				f.Close()
				return errors.New("download too large")
			}
			if progress != nil && time.Since(last) > 200*time.Millisecond {
				progress(done, total)
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	if progress != nil {
		progress(done, total)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s does not match SHA256SUMS", a.Name)
	}
	return nil
}

// replaceFile puts a new binary at path. On Unix the running program can be replaced in place; on
// Windows it is renamed out of the way first (Cleanup removes it at the next start).
func replaceFile(src, path string) error {
	mode := os.FileMode(0o755)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.Chmod(src, mode); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		old := path + ".old"
		_ = os.Remove(old)
		if err := os.Rename(path, old); err != nil {
			return err
		}
		if err := moveFile(src, path); err != nil {
			_ = os.Rename(old, path)
			return err
		}
		return nil
	}
	return moveFile(src, path)
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// installMacApp unpacks Quay.app next to the running one, checks it reports the new version, and
// swaps the two bundles (the running process keeps its files).
func installMacApp(ctx context.Context, zipFile, stage, app, tag string) error {
	if out, err := exec.CommandContext(ctx, "ditto", "-x", "-k", zipFile, stage).CombinedOutput(); err != nil {
		return fmt.Errorf("unzip: %v %s", err, out)
	}
	fresh := filepath.Join(stage, "Quay.app")
	out, err := exec.CommandContext(ctx, filepath.Join(fresh, "Contents", "MacOS", "quay"), "--version").Output()
	if err != nil || !strings.Contains(string(out), strings.TrimPrefix(tag, "v")) {
		return fmt.Errorf("the downloaded Quay.app does not run as %s (%v)", tag, err)
	}
	old := filepath.Join(filepath.Dir(app), ".Quay-old-"+randHex()+".app")
	if err := os.Rename(app, old); err != nil {
		return fmt.Errorf("cannot replace %s: %w", app, err)
	}
	if err := os.Rename(fresh, app); err != nil {
		_ = os.Rename(old, app)
		return err
	}
	_ = os.RemoveAll(old) // the running program keeps its open files
	return nil
}

// installWinZip unpacks the Windows zip (Quay\…) and puts each file in the installation folder.
func installWinZip(zipFile, stage, root string) error {
	zr, err := zip.OpenReader(zipFile)
	if err != nil {
		return err
	}
	defer zr.Close()
	var files []string
	for _, f := range zr.File {
		rel := strings.TrimPrefix(filepath.ToSlash(f.Name), "Quay/")
		if f.FileInfo().IsDir() || rel == f.Name || strings.Contains(rel, "..") {
			continue
		}
		dst := filepath.Join(stage, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := extract(f, dst); err != nil {
			return err
		}
		files = append(files, rel)
	}
	if !exists(filepath.Join(stage, "Quay.exe")) {
		return errors.New("the zip has no Quay\\Quay.exe")
	}
	for _, rel := range files {
		dst := filepath.Join(root, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := replaceFile(filepath.Join(stage, filepath.FromSlash(rel)), dst); err != nil {
			return err
		}
	}
	return nil
}

func extract(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(rc, 1<<30)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// untarBinary takes quay-<tag>-linux-<arch>/quay out of the .tar.gz.
func untarBinary(file, stage string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", errors.New("the archive has no quay binary")
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "quay" && strings.Count(strings.Trim(h.Name, "/"), "/") == 1 {
			dst := filepath.Join(stage, "quay")
			out, err := os.Create(dst)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, 1<<30)); err != nil {
				out.Close()
				return "", err
			}
			return dst, out.Close()
		}
	}
}

func randHex() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Cleanup removes what an update left behind: the replaced files (*.old) on Windows, an old
// bundle (.Quay-old-*.app) next to Quay.app on macOS.
func Cleanup() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if runtime.GOOS == "darwin" {
		if i := strings.Index(exe, ".app/Contents/MacOS/"); i > 0 {
			olds, _ := filepath.Glob(filepath.Join(filepath.Dir(exe[:i+4]), ".Quay-old-*.app"))
			for _, d := range olds {
				_ = os.RemoveAll(d)
			}
		}
		return
	}
	if runtime.GOOS != "windows" {
		return
	}
	for _, dir := range []string{filepath.Dir(exe), filepath.Join(filepath.Dir(exe), "bin"), filepath.Dir(filepath.Dir(exe))} {
		olds, _ := filepath.Glob(filepath.Join(dir, "*.old"))
		for _, f := range olds {
			_ = os.Remove(f)
		}
	}
}
