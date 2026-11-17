package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"LuwuDynamics/xgoduck_runtime_arduino": "https://github.com/LuwuDynamics/xgoduck_runtime_arduino",
		"github.com/a/b":                       "https://github.com/a/b",
		"https://gitlab.com/a/b.git":           "https://gitlab.com/a/b.git",
		"git@github.com:a/b.git":               "git@github.com:a/b.git",
	} {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q", in, got)
		}
	}
}

func TestFromDir(t *testing.T) {
	d := t.TempDir()
	app := filepath.Join(d, "My App!")
	os.MkdirAll(filepath.Join(app, "python"), 0o755)
	os.MkdirAll(filepath.Join(app, "data"), 0o755)
	os.MkdirAll(filepath.Join(app, ".cache", "sketch"), 0o755)
	os.WriteFile(filepath.Join(app, "app.yaml"), []byte("name: Demo\nicon: 🧪\n"), 0o644)
	os.WriteFile(filepath.Join(app, "python", "main.py"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(app, "data", "secret.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(app, ".cache", "sketch", "x.bin"), []byte("x"), 0o644)
	b, err := FromDir(app)
	if err != nil {
		t.Fatal(err)
	}
	if b.Files != 2 || b.App.Name != "Demo" || b.Folder != "My_App" {
		t.Fatalf("bundle: files=%d app=%+v folder=%q", b.Files, b.App, b.Folder)
	}
	os.Remove(filepath.Join(app, "python", "main.py"))
	if _, err := FromDir(app); err == nil {
		t.Fatal("missing python/main.py must be rejected")
	}
}

func TestAncestry(t *testing.T) {
	d := t.TempDir()
	repo, err := git.PlainInit(d, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	sig := &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}
	commit := func(msg string) string {
		os.WriteFile(filepath.Join(d, "f"), []byte(msg), 0o644)
		wt.Add("f")
		h, err := wt.Commit(msg, &git.CommitOptions{Author: sig})
		if err != nil {
			t.Fatal(err)
		}
		return h.String()
	}
	c1, c2 := commit("one"), commit("two")
	// A side branch from c1 that c2 does not hold.
	if err := wt.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(c1), Branch: "refs/heads/side", Create: true}); err != nil {
		t.Fatal(err)
	}
	side := commit("side")
	at := Ancestry("local:"+d, c2)
	for _, c := range []struct {
		from string
		want bool
	}{{c1, true}, {c2, true}, {side, false}, {strings.Repeat("e", 40), false}} {
		if got, err := at(c.from); err != nil || got != c.want {
			t.Errorf("Ancestry(%.7s of c2) = %v, %v", c.from, got, err)
		}
	}
	if _, err := Ancestry("local:"+d, strings.Repeat("e", 40))(c1); err == nil {
		t.Error("a commit not in the history must be an error")
	}
	if Ancestry("https://example.com/x.git", c2) != nil {
		t.Error("no cache: no history")
	}
}

func TestWithout(t *testing.T) {
	app := t.TempDir()
	for name, size := range map[string]int{"app.yaml": 12, "python/main.py": 1, "assembly/deck.pptx": 5000, "packages/app.zip": 3000, "docs/a.md": 7} {
		os.MkdirAll(filepath.Join(app, filepath.Dir(name)), 0o755)
		data := []byte(strings.Repeat("x", size))
		if name == "app.yaml" {
			data = []byte("name: Demo\n#2345\n")[:size]
		}
		os.WriteFile(filepath.Join(app, name), data, 0o644)
	}
	b, err := FromDir(app)
	if err != nil {
		t.Fatal(err)
	}
	skip := func(n string) bool {
		return n == "app.yaml" || strings.HasPrefix(n, "assembly/") || strings.HasPrefix(n, "packages/")
	}
	c, err := b.Without(skip)
	if err != nil {
		t.Fatal(err)
	}
	if c.Files != 3 || c.LeftOut != 2 || c.LeftOutBytes != 8000 || b.Files != 5 || b.LeftOut != 0 {
		t.Fatalf("files %d, left out %d (%d B); the original has %d", c.Files, c.LeftOut, c.LeftOutBytes, b.Files)
	}
	if len(c.Tar) >= len(b.Tar)-8000 || validate(c) != nil {
		t.Fatalf("tar %d → %d bytes, or no longer an app", len(b.Tar), len(c.Tar))
	}
	// Leaving out again what is already out changes nothing.
	if d, err := c.Without(skip); err != nil || d.LeftOut != 2 || d.Files != 3 {
		t.Fatalf("again: %+v %v", d, err)
	}
}
