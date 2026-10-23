package policystore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/caoxg/quay/internal/onnxinfo"
	"github.com/caoxg/quay/internal/profile"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestParse(t *testing.T) {
	good := strings.Repeat("ab", 32)
	ix, errs := Parse([]byte(fmt.Sprintf(`version: 1
name: Test
policies:
  - {id: walk-a, app: xgoduck, slot: walk, name: A, author: me, url: "https://x/a.onnx", sha256: "%s", size: 10}
  - {id: walk-a, app: xgoduck, slot: walk, name: Dup, author: me, url: "https://x/a.onnx", sha256: "%s"}
  - {id: Bad ID, app: xgoduck, slot: walk, name: B, author: me, url: "https://x/b.onnx", sha256: "%s"}
  - {id: no-sha, app: xgoduck, slot: walk, name: C, author: me, url: "https://x/c.onnx", sha256: "abc"}
  - {id: ftp, app: xgoduck, slot: walk, name: D, author: me, url: "ftp://x/d.onnx", sha256: "%s"}
  - {id: no-author, app: xgoduck, slot: walk, name: E, url: "https://x/e.onnx", sha256: "%s"}
`, strings.ToUpper(good), good, good, good, good)), "test")
	if ix == nil || len(ix.Policies) != 1 || ix.Policies[0].SHA256 != good || ix.Policies[0].Index != "test" {
		t.Fatalf("index: %+v", ix)
	}
	if len(errs) != 5 {
		t.Fatalf("want 5 rejected entries, got %d: %v", len(errs), errs)
	}
	if _, errs := Parse([]byte("version: 2\n"), "x"); len(errs) != 1 || !strings.Contains(errs[0].Error(), "version 2") {
		t.Fatalf("version check: %v", errs)
	}
}

func TestLoadAndDownload(t *testing.T) {
	model := []byte("pretend this is an onnx model")
	var indexDown, primaryDown atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.yaml":
			if indexDown.Load() {
				http.Error(w, "down", 503)
				return
			}
			fmt.Fprintf(w, `version: 1
policies:
  - {id: walk-a, app: xgoduck, slot: walk, name: A, author: me, url: "http://%s/primary.onnx", mirrors: ["http://%s/mirror.onnx"], sha256: "%s", size: %d}
  - {id: walk-bad, app: xgoduck, slot: walk, name: Bad, author: me, url: "http://%s/mirror.onnx", sha256: "%s"}
  - {id: other, app: weather, slot: main, name: W, author: me, url: "http://%s/mirror.onnx", sha256: "%s"}
`, r.Host, r.Host, sum(model), len(model), r.Host, strings.Repeat("0", 64), r.Host, sum(model))
		case "/primary.onnx":
			if primaryDown.Load() {
				http.Error(w, "gone", 404)
				return
			}
			w.Write(model)
		case "/mirror.onnx":
			w.Write(model)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	s := &Store{Dir: t.TempDir()}
	src := []Source{{URL: srv.URL + "/missing.yaml", Mirrors: []string{srv.URL + "/index.yaml"}}}

	r := s.Load(ctx, src)
	if len(r.Entries) != 3 || len(r.Problems) != 0 {
		t.Fatalf("load: %+v", r)
	}
	if got := ForApp(r.Entries, "xgoduck"); len(got) != 2 {
		t.Fatalf("ForApp: %v", got)
	}
	indexDown.Store(true)
	r = s.Load(ctx, src)
	if len(r.Entries) != 3 || len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "showing the copy") {
		t.Fatalf("cache fallback: %+v", r)
	}

	e, err := Find(r.Entries, "walk-a")
	if err != nil {
		t.Fatal(err)
	}
	primaryDown.Store(true) // the mirror serves it
	var last int64
	p, err := s.Download(ctx, *e, func(done, total int64) { last = done })
	if err != nil || last != int64(len(model)) {
		t.Fatalf("download: %v (progress %d)", err, last)
	}
	if b, _ := os.ReadFile(p); string(b) != string(model) || filepath.Base(p) != sum(model)+".onnx" {
		t.Fatalf("cached file %s", p)
	}
	bad, _ := Find(r.Entries, "walk-bad")
	if _, err := s.Download(ctx, *bad, nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a wrong checksum must fail: %v", err)
	}
	short := *e
	short.Size = 5
	short.SHA256 = strings.Repeat("1", 64)
	if _, err := s.Download(ctx, short, nil); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("a wrong size must fail: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(s.Dir, "models", ".download-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	if _, err := Find(append(r.Entries, Entry{ID: "walk-a", Index: "other"}), "walk-a"); err == nil {
		t.Fatal("an ambiguous id must be an error")
	}
}

func TestCheck(t *testing.T) {
	p := profile.ByID("xgoduck").Policies
	stock := &onnxinfo.Model{
		Inputs:   []onnxinfo.Tensor{{Name: "obs", Shape: []int64{1, 61}}},
		Outputs:  []onnxinfo.Tensor{{Name: "actions", Shape: []int64{1, 14}}},
		Metadata: map[string]string{"joint_names": "a,b", "observation_names": "o", "default_joint_pos": "0"},
	}
	walk, roller := p.Slot("walk"), p.Slot("roller")
	if probs := Check(p, walk, stock, stock); len(probs) != 0 {
		t.Fatalf("the stock model must pass: %v", probs)
	}
	m := &onnxinfo.Model{
		Inputs:   []onnxinfo.Tensor{{Name: "obs", Shape: []int64{-1, 62}}},
		Outputs:  []onnxinfo.Tensor{{Name: "actions", Shape: []int64{1, 14}}},
		Metadata: map[string]string{"joint_names": "b,a"},
	}
	probs := strings.Join(Check(p, roller, m, stock), "\n")
	for _, want := range []string{"input obs is [?,62]", "joint_names is \"b,a\"", "observation_names is missing", "default_joint_pos is missing"} {
		if !strings.Contains(probs, want) {
			t.Errorf("missing %q in:\n%s", want, probs)
		}
	}
	m.Inputs[0].Shape = []int64{-1, 61}
	m.Metadata = map[string]string{"joint_names": "a,b", "observation_names": "o"}
	if probs := Check(p, walk, m, stock); len(probs) != 0 {
		t.Fatalf("a symbolic batch dimension must pass: %v", probs)
	}
	if probs := Check(p, walk, m, nil); len(probs) != 0 {
		t.Fatalf("without a stock model only shapes are checked: %v", probs)
	}
}
