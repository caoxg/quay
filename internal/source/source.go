// Package source turns "a git repository at some version" (or a local folder) into an app
// bundle: a tar of the app files plus what Quay needs to know about it. Pure Go (go-git).
package source

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	gitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"gopkg.in/yaml.v3"
)

// Bundle is an app ready to upload.
type Bundle struct {
	Repo     string    `json:"repo"`             // normalised clone URL, or local:/abs/path
	Ref      string    `json:"ref"`              // branch, tag or commit the user asked for
	Commit   string    `json:"commit"`           // full hash ("" for a non-git folder)
	Subject  string    `json:"subject"`          // first line of the commit message
	Date     time.Time `json:"date"`             // commit date
	Subdir   string    `json:"subdir,omitempty"` // app root inside the repo
	Dirty    bool      `json:"dirty,omitempty"`  // local folder with uncommitted changes
	App      AppYAML   `json:"app"`              // from app.yaml
	QuayYAML []byte    `json:"-"`                // the app's quay.yaml, if any
	Folder   string    `json:"folder"`           // suggested folder name under ~/ArduinoApps
	Files    int       `json:"files"`
	Tar      []byte    `json:"-"`
}

// AppYAML is the part of Arduino's app manifest Quay shows.
type AppYAML struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Icon        string `yaml:"icon" json:"icon"`
}

// Ref is a branch or tag on a remote.
type Ref struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // branch | tag
	Commit string `json:"commit"`
}

// RemoteInfo is the result of listing a remote.
type RemoteInfo struct {
	URL           string `json:"url"`
	DefaultBranch string `json:"defaultBranch"`
	Refs          []Ref  `json:"refs"`
}

var shorthand = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// NormalizeURL accepts owner/repo, github.com/owner/repo, https and ssh URLs.
func NormalizeURL(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case shorthand.MatchString(s):
		return "https://github.com/" + strings.TrimSuffix(s, ".git")
	case strings.HasPrefix(s, "github.com/"), strings.HasPrefix(s, "gitlab.com/"), strings.HasPrefix(s, "gitee.com/"), strings.HasPrefix(s, "codeberg.org/"):
		return "https://" + s
	}
	return s
}

func auth(url string) transport.AuthMethod {
	if strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://") {
		if a, err := gitssh.NewSSHAgentAuth("git"); err == nil {
			return a
		}
	}
	if tok := os.Getenv("QUAY_GIT_TOKEN"); tok != "" && strings.HasPrefix(url, "https://") {
		return &githttp.BasicAuth{Username: "x-access-token", Password: tok}
	}
	return nil
}

// ListRefs lists branches and tags without cloning.
func ListRefs(ctx context.Context, url string) (*RemoteInfo, error) {
	url = NormalizeURL(url)
	rem := git.NewRemote(memory.NewStorage(), &gitcfg.RemoteConfig{Name: "origin", URLs: []string{url}})
	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: auth(url), PeelingOption: git.AppendPeeled})
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", url, err)
	}
	info := &RemoteInfo{URL: url}
	tags := map[string]Ref{}
	for _, r := range refs {
		n := r.Name()
		switch {
		case n == plumbing.HEAD && r.Type() == plumbing.SymbolicReference:
			info.DefaultBranch = r.Target().Short()
		case n.IsBranch():
			info.Refs = append(info.Refs, Ref{Name: n.Short(), Kind: "branch", Commit: r.Hash().String()})
		case n.IsTag():
			name := strings.TrimSuffix(n.Short(), "^{}")
			if strings.HasSuffix(n.String(), "^{}") || tags[name].Commit == "" { // peeled commit wins
				tags[name] = Ref{Name: name, Kind: "tag", Commit: r.Hash().String()}
			}
		}
	}
	for _, t := range tags {
		info.Refs = append(info.Refs, t)
	}
	sort.SliceStable(info.Refs, func(i, j int) bool {
		if info.Refs[i].Kind != info.Refs[j].Kind {
			return info.Refs[i].Kind == "branch"
		}
		if info.Refs[i].Kind == "tag" {
			return info.Refs[i].Name > info.Refs[j].Name // newest-looking tags first
		}
		return info.Refs[i].Name < info.Refs[j].Name
	})
	if info.DefaultBranch == "" {
		for _, cand := range []string{"main", "master"} {
			for _, r := range info.Refs {
				if r.Kind == "branch" && r.Name == cand {
					info.DefaultBranch = cand
				}
			}
		}
		if info.DefaultBranch == "" && len(info.Refs) > 0 && info.Refs[0].Kind == "branch" {
			info.DefaultBranch = info.Refs[0].Name
		}
	}
	return info, nil
}

// RemoteCommit returns the commit a branch or tag points to right now ("" if not a named ref).
func RemoteCommit(ctx context.Context, url, ref string) (string, error) {
	info, err := ListRefs(ctx, url)
	if err != nil {
		return "", err
	}
	if ref == "" {
		ref = info.DefaultBranch
	}
	for _, r := range info.Refs {
		if r.Name == ref {
			return r.Commit, nil
		}
	}
	return "", nil
}

// Fetch clones or updates the repository in cacheDir and bundles ref (empty = default branch).
func Fetch(ctx context.Context, cacheDir, url, ref, subdir string, progress io.Writer) (*Bundle, error) {
	url = NormalizeURL(url)
	sum := sha1.Sum([]byte(strings.ToLower(url)))
	dir := filepath.Join(cacheDir, hex.EncodeToString(sum[:8]))
	refspecs := []gitcfg.RefSpec{"+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"}

	repo, err := git.PlainOpen(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return nil, err
		}
		repo, err = git.PlainInitWithOptions(dir, &git.PlainInitOptions{Bare: true})
		if err != nil {
			return nil, err
		}
		if _, err := repo.CreateRemote(&gitcfg.RemoteConfig{Name: "origin", URLs: []string{url}, Fetch: refspecs}); err != nil {
			return nil, err
		}
	}
	err = repo.FetchContext(ctx, &git.FetchOptions{RemoteName: "origin", RefSpecs: refspecs, Auth: auth(url), Tags: git.AllTags, Force: true, Progress: progress})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}

	if ref == "" {
		info, err := ListRefs(ctx, url)
		if err != nil {
			return nil, err
		}
		if info.DefaultBranch == "" {
			return nil, fmt.Errorf("%s has no branches", url)
		}
		ref = info.DefaultBranch
	}
	hash, err := resolve(repo, ref)
	if err != nil {
		return nil, err
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	subdir = strings.Trim(path.Clean("/"+subdir), "/")
	if subdir != "" {
		if tree, err = tree.Tree(subdir); err != nil {
			return nil, fmt.Errorf("no folder %q in %s@%s", subdir, url, ref)
		}
	}
	b := &Bundle{Repo: url, Ref: ref, Commit: hash.String(), Subject: firstLine(commit.Message), Date: commit.Committer.When, Subdir: subdir, Folder: folderName(url, subdir)}
	if err := bundleTree(b, tree, commit.Committer.When); err != nil {
		return nil, err
	}
	return b, nil
}

func resolve(repo *git.Repository, ref string) (plumbing.Hash, error) {
	for _, n := range []string{"refs/remotes/origin/" + ref, "refs/tags/" + ref, "refs/heads/" + ref} {
		r, err := repo.Reference(plumbing.ReferenceName(n), true)
		if err != nil {
			continue
		}
		h := r.Hash()
		if tag, err := repo.TagObject(h); err == nil { // annotated tag
			c, err := tag.Commit()
			if err != nil {
				return plumbing.ZeroHash, err
			}
			return c.Hash, nil
		}
		return h, nil
	}
	if h, err := repo.ResolveRevision(plumbing.Revision(ref)); err == nil {
		return *h, nil
	}
	return plumbing.ZeroHash, fmt.Errorf("unknown branch, tag or commit %q", ref)
}

func bundleTree(b *Bundle, tree *object.Tree, when time.Time) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := tree.Files().ForEach(func(f *object.File) error {
		hdr := &tar.Header{Name: f.Name, ModTime: when, Mode: 0o644, Size: f.Size, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		switch f.Mode {
		case filemode.Executable:
			hdr.Mode = 0o755
		case filemode.Symlink:
			target, err := f.Contents()
			if err != nil {
				return err
			}
			hdr.Typeflag, hdr.Linkname, hdr.Size, hdr.Mode = tar.TypeSymlink, target, 0, 0o777
			b.Files++
			return tw.WriteHeader(hdr)
		}
		r, err := f.Reader()
		if err != nil {
			return err
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if err := inspect(b, f.Name, data); err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(data)
		b.Files++
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	b.Tar = buf.Bytes()
	return validate(b)
}

func inspect(b *Bundle, name string, data []byte) error {
	switch name {
	case "app.yaml":
		if err := yaml.Unmarshal(data, &b.App); err != nil {
			return fmt.Errorf("app.yaml: %w", err)
		}
	case "quay.yaml":
		b.QuayYAML = data
	}
	return nil
}

func validate(b *Bundle) error {
	r := tar.NewReader(bytes.NewReader(b.Tar))
	hasApp, hasMain := false, false
	for {
		h, err := r.Next()
		if err != nil {
			break
		}
		switch h.Name {
		case "app.yaml":
			hasApp = true
		case "python/main.py":
			hasMain = true
		}
	}
	if !hasApp {
		where := b.Repo
		if b.Subdir != "" {
			where += " / " + b.Subdir
		}
		return fmt.Errorf("%s is not an Arduino App (no app.yaml at its root)", where)
	}
	if !hasMain {
		return errors.New("not an Arduino App: python/main.py is missing")
	}
	if strings.TrimSpace(b.App.Name) == "" {
		b.App.Name = b.Folder
	}
	return nil
}

// FromDir bundles a local folder (for developers). Build output and app data are skipped.
func FromDir(dir string) (*Bundle, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", dir)
	}
	b := &Bundle{Repo: "local:" + abs, Ref: "working tree", Date: time.Now(), Folder: sanitize(filepath.Base(abs))}
	if repo, err := git.PlainOpenWithOptions(abs, &git.PlainOpenOptions{DetectDotGit: true}); err == nil {
		if head, err := repo.Head(); err == nil {
			b.Commit = head.Hash().String()
			if head.Name().IsBranch() {
				b.Ref = head.Name().Short()
			}
			if c, err := repo.CommitObject(head.Hash()); err == nil {
				b.Subject, b.Date = firstLine(c.Message), c.Committer.When
			}
			if wt, err := repo.Worktree(); err == nil {
				if s, err := wt.Status(); err == nil {
					b.Dirty = !s.IsClean()
				}
			}
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(abs, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		base := d.Name()
		if d.IsDir() && (base == ".git" || base == ".cache" || base == "node_modules" || (rel == "data") || strings.HasPrefix(rel, "data.before-restore-")) {
			return filepath.SkipDir
		}
		if d.IsDir() || base == ".DS_Store" || rel == ".quay.json" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name, hdr.Uname, hdr.Gname, hdr.Uid, hdr.Gid, hdr.Format = rel, "", "", 0, 0, tar.FormatPAX
		if info.Mode()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			hdr.Linkname = target
			b.Files++
			return tw.WriteHeader(hdr)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := inspect(b, rel, data); err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(data)
		b.Files++
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	b.Tar = buf.Bytes()
	if err := validate(b); err != nil {
		return nil, err
	}
	return b, nil
}

func folderName(url, subdir string) string {
	name := path.Base(strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git"))
	if subdir != "" {
		name = path.Base(subdir)
	}
	return sanitize(name)
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitize(s string) string {
	s = strings.Trim(unsafeChars.ReplaceAllString(s, "_"), "._-")
	if s == "" {
		return "app"
	}
	return s
}

// SanitizeFolder exposes the folder-name rule.
func SanitizeFolder(s string) string { return sanitize(s) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
