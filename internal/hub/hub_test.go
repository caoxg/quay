package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuiltin(t *testing.T) {
	b := Builtin()
	if b.Source != "built in" || b.Index.Updated == "" || len(b.Index.Apps) == 0 || len(b.Index.Systems) == 0 {
		t.Fatalf("built-in copy: %+v", b.Index)
	}
	for _, f := range append(b.Each(b.Index.Apps), b.Each(b.Index.Systems)...) {
		if len(f.Data) == 0 {
			t.Errorf("%s is empty", f.Name)
		}
	}
	if Builtin() != b {
		t.Fatal("the built-in copy is read again every time")
	}
}

func TestLoaderPrefersTheNewerCopy(t *testing.T) {
	newer := map[string]string{
		"/index.yaml":            "schema: 1\nupdated: \"2099-01-01\"\napps: [apps/a.yaml]\nsystems: [systems/x.yaml]\n",
		"/apps/a.yaml":           "id: a\nname: A\n",
		"/systems/x.yaml":        "id: x-os\nname: X OS\ndetect: {files: [/etc/x-release]}\n",
		"/old/index.yaml":        "schema: 1\nupdated: \"2000-01-01\"\nsystems: [systems/x.yaml]\n",
		"/old/systems/x.yaml":    "id: x-os\nname: X OS\ndetect: {files: [/etc/x-release]}\n",
		"/future/index.yaml":     "schema: 9\nupdated: \"2099-01-01\"\nsystems: []\n",
		"/broken/index.yaml":     "schema: 1\nupdated: \"2099-01-01\"\nsystems: [systems/x.yaml]\n",
		"/broken/systems/x.yaml": "id: [\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := newer[r.URL.Path]; ok {
			w.Write([]byte(b))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	saved, savedM := IndexURL, IndexMirrors
	defer func() { IndexURL, IndexMirrors = saved, savedM }()
	l := &Loader{Dir: t.TempDir(), TTL: time.Hour}
	ctx := context.Background()

	IndexURL, IndexMirrors = srv.URL+"/missing/index.yaml", []string{srv.URL + "/index.yaml"}
	if !l.Stale() {
		t.Fatal("no copy yet, but not stale")
	}
	if err := l.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if s := l.Load(); s.Index.Updated != "2099-01-01" || len(s.Index.Systems) != 1 || len(s.Index.Apps) != 1 || !strings.HasSuffix(s.Source, "/index.yaml") ||
		!strings.Contains(string(s.Files["apps/a.yaml"]), "id: a") {
		t.Fatalf("fetched: %+v", s)
	}
	if l.Stale() {
		t.Fatal("stale right after a fetch")
	}

	for _, bad := range []string{"/future/index.yaml", "/broken/index.yaml"} {
		IndexURL, IndexMirrors = srv.URL+bad, nil
		if err := l.Refresh(ctx); err == nil {
			t.Fatalf("%s: accepted", bad)
		}
		if s := l.Load(); s.Index.Updated != "2099-01-01" {
			t.Fatalf("%s replaced the good copy", bad)
		}
	}

	IndexURL = srv.URL + "/old/index.yaml"
	if err := l.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if s := l.Load(); s.Source != "built in" {
		t.Fatalf("an older fetched copy won over the built-in one: %+v", s)
	}
	os.RemoveAll(l.Dir)
	if s := l.Load(); s.Source != "built in" {
		t.Fatal("no copy, but not the built-in one")
	}
}
