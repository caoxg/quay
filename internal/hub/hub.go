// Package hub keeps Quay's copy of the quay-profiles repository: app profiles (apps/, for apps
// that do not ship a quay.yaml) and system profiles (systems/, for board systems other than the
// stock image). They are published independently of Quay releases: a copy is built in, and a
// newer one fetched from the repository replaces it.
package hub

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/caoxg/quay/internal/version"
)

// The quay-profiles repository, read directly or through jsDelivr (faster in mainland China).
var (
	IndexURL     = "https://raw.githubusercontent.com/caoxg/quay-profiles/main/index.yaml"
	IndexMirrors = []string{"https://cdn.jsdelivr.net/gh/caoxg/quay-profiles@main/index.yaml"}
)

// Schema is the newest repository layout this Quay reads.
const Schema = 1

// Index is index.yaml at the top of the repository.
type Index struct {
	Schema  int      `yaml:"schema"`
	Updated string   `yaml:"updated"` // YYYY-MM-DD[THH:MM]: the newer copy wins
	Apps    []string `yaml:"apps"`    // app profiles, relative to index.yaml
	Systems []string `yaml:"systems"` // system profiles
}

// Snapshot is one copy of the repository: its index and files, and where it came from.
type Snapshot struct {
	Index  Index
	Files  map[string][]byte
	Source string // "built in", or the index URL
}

// Each returns the files of a list (Index.Apps or Index.Systems) in order, as name and content.
func (s *Snapshot) Each(list []string) []File {
	var out []File
	for _, f := range list {
		out = append(out, File{Name: f, Data: s.Files[f]})
	}
	return out
}

// File is one profile file.
type File struct {
	Name string
	Data []byte
}

// builtin is a copy of the repository taken when Quay was built (make profiles).
//
//go:embed builtin
var builtin embed.FS

var (
	builtinOnce sync.Once
	builtinSnap *Snapshot
)

// Builtin is the copy built into Quay.
func Builtin() *Snapshot {
	builtinOnce.Do(func() { builtinSnap = readBuiltin() })
	return builtinSnap
}

func readBuiltin() *Snapshot {
	files := map[string][]byte{}
	ix, err := readIndex(func(name string) ([]byte, error) { return builtin.ReadFile(path.Join("builtin", name)) }, files)
	if err != nil {
		panic("built-in profiles: " + err.Error())
	}
	return &Snapshot{Index: *ix, Files: files, Source: "built in"}
}

// readIndex reads index.yaml and the files it lists through get, into files.
func readIndex(get func(name string) ([]byte, error), files map[string][]byte) (*Index, error) {
	b, err := get("index.yaml")
	if err != nil {
		return nil, err
	}
	var ix Index
	if err := yaml.Unmarshal(b, &ix); err != nil {
		return nil, fmt.Errorf("index.yaml: %w", err)
	}
	if ix.Schema < 1 || ix.Schema > Schema {
		return nil, fmt.Errorf("index.yaml: schema %d (this Quay reads up to %d; update Quay)", ix.Schema, Schema)
	}
	for _, f := range append(append([]string{}, ix.Apps...), ix.Systems...) {
		if path.IsAbs(f) || strings.Contains(f, "..") || !strings.HasSuffix(f, ".yaml") {
			return nil, fmt.Errorf("index.yaml: bad file %q", f)
		}
		if files[f], err = get(f); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	return &ix, nil
}

// Loader keeps the newest copy of the repository in Dir.
type Loader struct {
	Dir    string
	Client *http.Client
	TTL    time.Duration // how old the copy may get before Current fetches again (default 12 h)
	Manual func() bool   // true: fetch only when asked (Refresh), never by itself

	mu     sync.Mutex
	last   time.Time // last fetch attempt
	loaded *Snapshot // what Load returned, for the copy last read (modTime)
	mod    time.Time
}

type cached struct {
	Source  string            `json:"source"`
	Fetched time.Time         `json:"fetched"`
	Index   []byte            `json:"index"`
	Files   map[string][]byte `json:"files"`
}

func (l *Loader) file() string { return filepath.Join(l.Dir, "systems.json") }

// Load returns the newer of the fetched copy and the built-in one (the same *Snapshot until the
// kept copy changes).
func (l *Loader) Load() *Snapshot {
	var mod time.Time
	if st, err := os.Stat(l.file()); err == nil {
		mod = st.ModTime()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loaded != nil && mod.Equal(l.mod) {
		return l.loaded
	}
	l.loaded, l.mod = l.load(), mod
	return l.loaded
}

func (l *Loader) load() *Snapshot {
	b := Builtin()
	c, err := l.readCache()
	if err != nil {
		return b
	}
	files := c.Files
	ix, err := readIndex(func(name string) ([]byte, error) {
		if name == "index.yaml" {
			return c.Index, nil
		}
		if v, ok := files[name]; ok {
			return v, nil
		}
		return nil, os.ErrNotExist
	}, map[string][]byte{})
	if err != nil || ix.Updated < b.Index.Updated {
		return b
	}
	return &Snapshot{Index: *ix, Files: files, Source: c.Source}
}

func (l *Loader) readCache() (*cached, error) {
	b, err := os.ReadFile(l.file())
	if err != nil {
		return nil, err
	}
	var c cached
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Stale reports whether the copy is older than TTL (or missing) and no fetch was tried in the last
// hour (by any Quay: a failed fetch, e.g. where GitHub is slow, is not retried by every command).
func (l *Loader) Stale() bool {
	ttl := l.TTL
	if ttl == 0 {
		ttl = 12 * time.Hour
	}
	l.mu.Lock()
	tried := time.Since(l.last) < time.Hour
	l.mu.Unlock()
	if st, err := os.Stat(filepath.Join(l.Dir, "tried")); tried || (err == nil && time.Since(st.ModTime()) < time.Hour) {
		return false
	}
	c, err := l.readCache()
	return err != nil || time.Since(c.Fetched) > ttl
}

// Refresh fetches the repository (the first of IndexURL and its mirrors that answers) and keeps it
// when it reads cleanly.
func (l *Loader) Refresh(ctx context.Context) error {
	l.mu.Lock()
	l.last = time.Now()
	l.mu.Unlock()
	if os.MkdirAll(l.Dir, 0o755) == nil {
		_ = os.WriteFile(filepath.Join(l.Dir, "tried"), nil, 0o644)
	}
	var first error
	for _, u := range append([]string{IndexURL}, IndexMirrors...) {
		base := u[:strings.LastIndex(u, "/")+1]
		files := map[string][]byte{}
		var index []byte
		_, err := readIndex(func(name string) ([]byte, error) {
			b, err := l.get(ctx, base+name)
			if name == "index.yaml" {
				index = b
			}
			return b, err
		}, files)
		if err == nil {
			for f, b := range files {
				var v map[string]any
				if yerr := yaml.Unmarshal(b, &v); yerr != nil {
					err = fmt.Errorf("%s: %v", f, yerr)
				}
			}
		}
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		b, _ := json.Marshal(cached{Source: u, Fetched: time.Now(), Index: index, Files: files})
		if err := os.MkdirAll(l.Dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(l.file(), b, 0o644)
	}
	return first
}

func (l *Loader) get(ctx context.Context, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Quay/"+version.Version)
	c := l.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err == nil && len(b) > 1<<20 {
		err = errors.New(u + ": too large")
	}
	return b, err
}

func (l *Loader) manual() bool { return l.Manual != nil && l.Manual() }

// Auto reports whether the loader fetches newer copies by itself.
func (l *Loader) Auto() bool { return !l.manual() }

// Default is the loader Get and Current use; nil means the built-in copy only. The CLI and the app
// set it.
var Default *Loader

// Get returns the copy to use now, without fetching. QUAY_PROFILES=DIR uses a checkout of the
// repository instead (for working on profiles).
func Get() *Snapshot {
	if dir := os.Getenv("QUAY_PROFILES"); dir != "" {
		files := map[string][]byte{}
		ix, err := readIndex(func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))) }, files)
		if err == nil {
			return &Snapshot{Index: *ix, Files: files, Source: dir}
		}
		fmt.Fprintln(os.Stderr, "QUAY_PROFILES:", err)
	}
	if Default == nil {
		return Builtin()
	}
	return Default.Load()
}

// Current is Get after fetching a fresh copy when the kept one is stale (for at most a few
// seconds; failing that, the kept or built-in copy is used).
func Current(ctx context.Context) *Snapshot {
	if Default != nil && os.Getenv("QUAY_PROFILES") == "" && !Default.manual() && Default.Stale() {
		ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		_ = Default.Refresh(ctx)
		cancel()
	}
	return Get()
}
