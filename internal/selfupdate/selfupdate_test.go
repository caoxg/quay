package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		v, cur string
		want   bool
	}{
		{"v0.2.0", "v0.1.9", true}, {"v0.1.10", "v0.1.9", true}, {"v1.0.0", "v0.9.9", true},
		{"v0.1.0", "v0.1.0", false}, {"v0.1.0", "v0.2.0", false}, {"v0.2.0", "dev", false},
		{"v0.2.0", "v0.2.0-rc1", true}, {"v0.2.0-rc1", "v0.1.9", true}, {"v0.2.0-rc1", "v0.2.0", false}, {"latest", "v0.1.0", false},
		{"v0.1.0", "v0.1.0-3-gd887a3a-dirty", false}, {"v0.1.0", "v0.1.0-dirty", false}, {"v0.1.1", "v0.1.0-3-gd887a3a", true},
	} {
		if got := Newer(c.v, c.cur); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.v, c.cur, got)
		}
	}
}

// fakeGitHub serves a release of tag with the given files, SHA256SUMS signed by key (nil: unsigned).
func fakeGitHub(t *testing.T, tag string, files map[string][]byte, key ed25519.PrivateKey, apiDown bool) *httptest.Server {
	t.Helper()
	var sums strings.Builder
	for name, b := range files {
		h := sha256.Sum256(b)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), name)
	}
	all := map[string][]byte{"SHA256SUMS": []byte(sums.String())}
	if key != nil {
		all["SHA256SUMS.sig"] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, all["SHA256SUMS"])))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/"+Repo+"/releases/latest":
			if apiDown {
				http.Error(w, "rate limited", http.StatusForbidden)
				return
			}
			fmt.Fprintf(w, `{"tag_name": %q, "name": "Quay %s", "body": "## What changed\n- things", "html_url": "x", "assets": [`, tag, tag)
			first := true
			for name, b := range files {
				if !first {
					fmt.Fprint(w, ",")
				}
				first = false
				fmt.Fprintf(w, `{"name": %q, "size": %d, "browser_download_url": "%s/download/%s"}`, name, len(b), "http://"+r.Host, name)
			}
			fmt.Fprint(w, "]}")
		case r.URL.Path == "/"+Repo+"/releases/latest":
			w.Header().Set("Location", "http://"+r.Host+"/"+Repo+"/releases/tag/"+tag)
			w.WriteHeader(http.StatusFound)
		default:
			name := filepath.Base(r.URL.Path)
			if b, ok := files[name]; ok {
				w.Write(b)
			} else if b, ok := all[name]; ok {
				w.Write(b)
			} else {
				http.NotFound(w, r)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setup(t *testing.T) (ed25519.PrivateKey, string) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	savedKeys, savedAPI, savedWeb, savedNow := PublicKeys, API, Web, versionNow
	t.Cleanup(func() { PublicKeys, API, Web, versionNow = savedKeys, savedAPI, savedWeb, savedNow })
	PublicKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	versionNow = func() string { return "v0.1.0" }
	exe := filepath.Join(t.TempDir(), "quay")
	if err := os.WriteFile(exe, []byte("old program"), 0o755); err != nil {
		t.Fatal(err)
	}
	return priv, exe
}

func TestInstallBinary(t *testing.T) {
	key, exe := setup(t)
	name := AssetName(KindBinary, "v0.2.0")
	srv := fakeGitHub(t, "v0.2.0", map[string][]byte{name: []byte("new program"), "other": []byte("x")}, key, false)
	API, Web = srv.URL, srv.URL
	u := &Updater{Exe: exe}
	rel, err := u.Latest(context.Background())
	if err != nil || rel.Version != "v0.2.0" || !strings.Contains(rel.Notes, "What changed") || rel.Assets[name].Size != 11 {
		t.Fatalf("latest: %v %+v", err, rel)
	}
	if !(&Check{Latest: rel}).Available() {
		t.Fatal("v0.2.0 is not newer than v0.1.0")
	}
	var seen int64
	res, err := u.Install(context.Background(), rel, func(done, total int64) { seen = done })
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new program" || !res.Relaunch || seen != 11 {
		t.Fatalf("installed %q %+v (progress %d)", b, res, seen)
	}
	if st, _ := os.Stat(exe); st.Mode().Perm()&0o100 == 0 {
		t.Fatal("not executable")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".quay-update-*")); len(left) > 0 {
		t.Fatalf("staging left: %v", left)
	}
}

func TestLatestWithoutTheAPI(t *testing.T) {
	key, exe := setup(t)
	name := AssetName(KindBinary, "v0.3.0")
	srv := fakeGitHub(t, "v0.3.0", map[string][]byte{name: []byte("newer")}, key, true)
	API, Web = srv.URL, srv.URL
	u := &Updater{Exe: exe}
	rel, err := u.Latest(context.Background())
	if err != nil || rel.Version != "v0.3.0" || rel.Notes != "" {
		t.Fatalf("latest from the redirect: %v %+v", err, rel)
	}
	if _, err := u.Install(context.Background(), rel, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "newer" {
		t.Fatalf("installed %q", b)
	}
}

func TestRefusesWhatIsNotSignedOrDoesNotMatch(t *testing.T) {
	key, exe := setup(t)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	name := AssetName(KindBinary, "v0.2.0")
	for what, srv := range map[string]*httptest.Server{
		"unsigned":       fakeGitHub(t, "v0.2.0", map[string][]byte{name: []byte("evil")}, nil, false),
		"another key":    fakeGitHub(t, "v0.2.0", map[string][]byte{name: []byte("evil")}, other, false),
		"no file for us": fakeGitHub(t, "v0.2.0", map[string][]byte{"something-else": []byte("x")}, key, false),
	} {
		API, Web = srv.URL, srv.URL
		u := &Updater{Exe: exe}
		rel, err := u.Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := u.Install(context.Background(), rel, nil); err == nil {
			t.Errorf("%s: installed", what)
		}
		if b, _ := os.ReadFile(exe); string(b) != "old program" {
			t.Fatalf("%s: the program was replaced", what)
		}
	}

	// The file served is not the one SHA256SUMS lists.
	srv := fakeGitHub(t, "v0.2.0", map[string][]byte{name: []byte("good")}, key, false)
	API, Web = srv.URL, srv.URL
	u := &Updater{Exe: exe}
	rel, _ := u.Latest(context.Background())
	rel.Assets[name] = Asset{Name: name, URL: srv.URL + "/download/SHA256SUMS"}
	if _, err := u.Install(context.Background(), rel, nil); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a swapped file: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old program" {
		t.Fatal("the program was replaced by a swapped file")
	}
}

func TestUnpackers(t *testing.T) {
	dir := t.TempDir()
	// The Linux .tar.gz: quay-<tag>-linux-<arch>/quay among other files.
	var tb bytes.Buffer
	gz := gzip.NewWriter(&tb)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"quay-v0.2.0-linux-arm64/README.md": "r", "quay-v0.2.0-linux-arm64/quay": "linux app"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	tgz := filepath.Join(dir, "q.tar.gz")
	os.WriteFile(tgz, tb.Bytes(), 0o644)
	bin, err := untarBinary(tgz, dir)
	if b, _ := os.ReadFile(bin); err != nil || string(b) != "linux app" {
		t.Fatalf("tar: %v %q", err, b)
	}

	// The Windows .zip: Quay\Quay.exe and Quay\bin\quay.exe, into an installed folder.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, body := range map[string]string{"Quay/Quay.exe": "new app", "Quay/bin/quay.exe": "new cli", "Quay/README.md": "readme"} {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	zf := filepath.Join(dir, "q.zip")
	os.WriteFile(zf, zb.Bytes(), 0o644)
	dir = t.TempDir() // a new folder: "quay" from the tar and "Quay" are one name on macOS
	zf2 := filepath.Join(dir, "q.zip")
	os.Rename(zf, zf2)
	zf = zf2
	root := filepath.Join(dir, "Quay")
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	os.WriteFile(filepath.Join(root, "Quay.exe"), []byte("old app"), 0o755)
	stage := filepath.Join(dir, "stage")
	os.MkdirAll(stage, 0o755)
	if err := installWinZip(zf, stage, root); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"Quay.exe": "new app", "bin/quay.exe": "new cli", "README.md": "readme"} {
		if b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(name))); string(b) != want {
			t.Errorf("%s: %q", name, b)
		}
	}
}

func TestCheckFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "update.json")
	if LoadCheck(file) != nil {
		t.Fatal("a check from nowhere")
	}
	c := &Check{Current: "v0.1.0", Latest: &Release{Version: "v0.2.0", Notes: "n"}}
	if err := c.Save(file); err != nil {
		t.Fatal(err)
	}
	if got := LoadCheck(file); got == nil || got.Latest.Version != "v0.2.0" || got.Latest.Notes != "n" {
		t.Fatalf("loaded %+v", got)
	}
}
