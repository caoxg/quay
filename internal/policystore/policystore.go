// Package policystore is the computer side of Quay's policy catalog: it reads policy indexes (a YAML
// file anyone can host, e.g. in a GitHub repository), downloads the models they list, verifies them
// (SHA-256, size) and checks a model against what an app's profile declares (input and output shapes,
// metadata) before anything is uploaded to a board.
package policystore

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/caoxg/quay/internal/onnxinfo"
	"github.com/caoxg/quay/internal/profile"
	"github.com/caoxg/quay/internal/version"
)

// Source is where an index is published.
type Source struct {
	URL     string   `json:"url"`
	Mirrors []string `json:"mirrors,omitempty"`
}

// Sources lists the indexes to read: the community indexes the profiles name, then the user's own.
func Sources(profiles []*profile.Profile, own []string) []Source {
	var out []Source
	seen := map[string]bool{}
	for _, p := range profiles {
		if p == nil || p.Policies == nil || p.Policies.Index == nil || seen[p.Policies.Index.URL] {
			continue
		}
		seen[p.Policies.Index.URL] = true
		out = append(out, Source{URL: p.Policies.Index.URL, Mirrors: p.Policies.Index.Mirrors})
	}
	for _, u := range own {
		if !seen[u] {
			seen[u] = true
			out = append(out, Source{URL: u})
		}
	}
	return out
}

// Index is one policy index file.
type Index struct {
	Version  int     `yaml:"version" json:"version"`
	Name     string  `yaml:"name" json:"name"`
	Policies []Entry `yaml:"policies" json:"policies"`
}

// Entry is one published policy.
type Entry struct {
	ID            string   `yaml:"id" json:"id"`
	App           string   `yaml:"app" json:"app"`   // profile id, e.g. xgoduck
	Slot          string   `yaml:"slot" json:"slot"` // slot id in that profile
	Name          string   `yaml:"name" json:"name"`
	NameZH        string   `yaml:"name_zh" json:"nameZh,omitempty"`
	Author        string   `yaml:"author" json:"author"`
	Description   string   `yaml:"description" json:"description,omitempty"`
	DescriptionZH string   `yaml:"description_zh" json:"descriptionZh,omitempty"`
	URL           string   `yaml:"url" json:"url"`
	Mirrors       []string `yaml:"mirrors" json:"mirrors,omitempty"`
	SHA256        string   `yaml:"sha256" json:"sha256"`
	Size          int64    `yaml:"size" json:"size"`
	License       string   `yaml:"license" json:"license,omitempty"`
	Trainer       string   `yaml:"trainer" json:"trainer,omitempty"` // training code and version, e.g. "xgoduck_rl afd091b"
	Homepage      string   `yaml:"homepage" json:"homepage,omitempty"`
	Video         string   `yaml:"video" json:"video,omitempty"`
	Created       string   `yaml:"created" json:"created,omitempty"` // YYYY-MM-DD

	Index string `yaml:"-" json:"index"` // the index it was read from
}

var (
	entryID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	hexSHA  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// MaxModel bounds a download; policies for small robots are a few MB.
const MaxModel = 256 << 20

// Parse reads an index. Entries that are incomplete are dropped and reported, so one bad entry does
// not hide the rest.
func Parse(b []byte, source string) (*Index, []error) {
	var ix Index
	if err := yaml.Unmarshal(b, &ix); err != nil {
		return nil, []error{fmt.Errorf("%s: %w", source, err)}
	}
	if ix.Version != 1 {
		return nil, []error{fmt.Errorf("%s: unsupported index version %d (this Quay reads version 1)", source, ix.Version)}
	}
	var errs []error
	seen := map[string]bool{}
	ok := ix.Policies[:0]
	for _, e := range ix.Policies {
		e.SHA256 = strings.ToLower(strings.TrimSpace(e.SHA256))
		e.Index = source
		switch {
		case !entryID.MatchString(e.ID) || seen[e.ID]:
			errs = append(errs, fmt.Errorf("%s: entry %q: id must be unique and use a-z, 0-9, '.', '-' or '_'", source, e.ID))
		case e.App == "" || e.Slot == "" || e.Name == "" || e.Author == "":
			errs = append(errs, fmt.Errorf("%s: entry %s: app, slot, name and author are required", source, e.ID))
		case !hexSHA.MatchString(e.SHA256):
			errs = append(errs, fmt.Errorf("%s: entry %s: sha256 must be 64 hex digits", source, e.ID))
		case !httpURL(e.URL):
			errs = append(errs, fmt.Errorf("%s: entry %s: url must be http(s)", source, e.ID))
		case e.Size < 0 || e.Size > MaxModel:
			errs = append(errs, fmt.Errorf("%s: entry %s: bad size", source, e.ID))
		default:
			seen[e.ID] = true
			ok = append(ok, e)
		}
	}
	ix.Policies = ok
	return &ix, errs
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// Store reads indexes and keeps downloads in Dir.
type Store struct {
	Dir    string
	Client *http.Client
}

func (s *Store) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// Result is what Load found: the entries of every index that could be read, and a note per index
// that could not (or that came from the cache).
type Result struct {
	Entries  []Entry  `json:"entries"`
	Problems []string `json:"problems"`
}

// Load reads the indexes, newest first. An index that cannot be fetched falls back to its last good
// copy in the cache.
func (s *Store) Load(ctx context.Context, sources []Source) *Result {
	r := &Result{Entries: []Entry{}, Problems: []string{}}
	for _, src := range sources {
		b, err := s.fetchIndex(ctx, src)
		cache := filepath.Join(s.Dir, "index-"+shortHash(src.URL)+".yaml")
		if err != nil {
			cached, cerr := os.ReadFile(cache)
			if cerr != nil {
				r.Problems = append(r.Problems, err.Error())
				continue
			}
			when := ""
			if st, err := os.Stat(cache); err == nil {
				when = " from " + st.ModTime().Format("2006-01-02 15:04")
			}
			r.Problems = append(r.Problems, fmt.Sprintf("%v; showing the copy%s", err, when))
			b = cached
		}
		ix, errs := Parse(b, src.URL)
		for _, e := range errs {
			r.Problems = append(r.Problems, e.Error())
		}
		if ix == nil {
			continue
		}
		if err == nil {
			_ = os.MkdirAll(s.Dir, 0o755)
			_ = os.WriteFile(cache, b, 0o644)
		}
		r.Entries = append(r.Entries, ix.Policies...)
	}
	return r
}

func (s *Store) fetchIndex(ctx context.Context, src Source) ([]byte, error) {
	var first error
	for _, u := range append([]string{src.URL}, src.Mirrors...) {
		b, err := s.get(ctx, u, 4<<20)
		if err == nil {
			return b, nil
		}
		if first == nil {
			first = err
		}
	}
	return nil, first
}

// get reads a URL (or a local file, for indexes kept on disk) up to limit bytes.
func (s *Store) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	if p, ok := localPath(u); ok {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, limit))
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := s.open(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = errors.New("too large")
	}
	return b, err
}

func (s *Store) open(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Quay/"+version.Version)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return resp, nil
}

// localPath accepts file:// URLs and plain paths.
func localPath(u string) (string, bool) {
	if strings.HasPrefix(u, "file://") {
		p, err := url.Parse(u)
		if err == nil {
			return p.Path, true
		}
	}
	if !strings.Contains(u, "://") {
		return u, true
	}
	return "", false
}

// Download fetches an entry's model into the cache (named by its SHA-256) and verifies it. The
// mirrors are tried in turn. progress, when set, gets the bytes done and the total (-1 if unknown).
func (s *Store) Download(ctx context.Context, e Entry, progress func(done, total int64)) (string, error) {
	dst := filepath.Join(s.Dir, "models", e.SHA256+".onnx")
	if sum, _, err := HashFile(dst); err == nil && sum == e.SHA256 {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	var errs []error
	for _, u := range append([]string{e.URL}, e.Mirrors...) {
		err := s.download(ctx, u, e, dst, progress)
		if err == nil {
			return dst, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		errs = append(errs, err)
	}
	return "", errors.Join(errs...)
}

func (s *Store) download(ctx context.Context, u string, e Entry, dst string, progress func(done, total int64)) error {
	resp, err := s.open(ctx, u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	total := resp.ContentLength
	if e.Size > 0 {
		total = e.Size
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	limit := int64(MaxModel)
	if e.Size > 0 {
		limit = e.Size
	}
	w := io.MultiWriter(tmp, h)
	var done int64
	buf := make([]byte, 64<<10)
	r := io.LimitReader(resp.Body, limit+1)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				tmp.Close()
				return err
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return fmt.Errorf("%s: %w", u, rerr)
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if done > limit || (e.Size > 0 && done != e.Size) {
		return fmt.Errorf("%s: got %d bytes, the index says %d", u, done, e.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != e.SHA256 {
		return fmt.Errorf("%s: checksum mismatch (got %s…, the index says %s…)", u, sum[:12], e.SHA256[:12])
	}
	return os.Rename(tmp.Name(), dst)
}

// HashFile returns a file's SHA-256 and size.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func shortHash(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:6])
}

// Check compares a model with what the profile declares for the slot. stock is the model the app
// shipped for that slot (nil when unknown); keys in MatchMetadata must carry the same value. Each
// returned string is one reason not to install it.
func Check(p *profile.Policies, slot *profile.Slot, m, stock *onnxinfo.Model) []string {
	var out []string
	if len(m.Inputs) == 0 || len(m.Outputs) == 0 {
		return []string{"the model has no inputs or no outputs"}
	}
	if len(p.Input) > 0 && !onnxinfo.SameShape(m.Inputs[0].Shape, p.Input) {
		out = append(out, fmt.Sprintf("input %s is %s, the app feeds %s", m.Inputs[0].Name, onnxinfo.ShapeString(m.Inputs[0].Shape), onnxinfo.ShapeString(p.Input)))
	}
	if len(p.Output) > 0 && !onnxinfo.SameShape(m.Outputs[0].Shape, p.Output) {
		out = append(out, fmt.Sprintf("output %s is %s, the app reads %s", m.Outputs[0].Name, onnxinfo.ShapeString(m.Outputs[0].Shape), onnxinfo.ShapeString(p.Output)))
	}
	for _, k := range slot.RequireMetadata {
		if strings.TrimSpace(m.Metadata[k]) == "" {
			out = append(out, fmt.Sprintf("metadata %s is missing; the %s slot needs it", k, slot.Name))
		}
	}
	if stock != nil {
		for _, k := range p.MatchMetadata {
			want, ok := stock.Metadata[k]
			if !ok {
				continue
			}
			got, has := m.Metadata[k]
			switch {
			case !has:
				out = append(out, fmt.Sprintf("metadata %s is missing (the stock model has %s)", k, clip(want)))
			case got != want:
				out = append(out, fmt.Sprintf("metadata %s is %s, the stock model has %s", k, clip(got), clip(want)))
			}
		}
	}
	return out
}

func clip(s string) string {
	if len(s) > 80 {
		return `"` + s[:77] + `…"`
	}
	return `"` + s + `"`
}

// ForApp keeps the entries for one profile, in index order.
func ForApp(entries []Entry, app string) []Entry {
	out := []Entry{}
	for _, e := range entries {
		if e.App == app {
			out = append(out, e)
		}
	}
	return out
}

// Find looks an entry up by id (or by an "INDEX#id" key when ids collide across indexes).
func Find(entries []Entry, id string) (*Entry, error) {
	var hits []Entry
	for _, e := range entries {
		if e.ID == id || e.Index+"#"+e.ID == id {
			hits = append(hits, e)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no policy %q in the indexes", id)
	case 1:
		return &hits[0], nil
	}
	return nil, fmt.Errorf("%d indexes list a policy %q; use INDEX_URL#%s", len(hits), id, id)
}
