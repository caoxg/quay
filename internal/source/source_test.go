package source

import (
	"os"
	"path/filepath"
	"testing"
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
