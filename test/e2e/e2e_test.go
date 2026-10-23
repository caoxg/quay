//go:build e2e

// End-to-end test against the fake board in test/fakeboard (Docker), deploying the real
// XGO-Duck runtime from GitHub. Run with: make e2e
package e2e

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caoxg/quay/internal/board"
	"github.com/caoxg/quay/internal/core"
	"github.com/caoxg/quay/internal/mirror"
	"github.com/caoxg/quay/internal/relay"
)

const (
	container = "quay-fakeboard"
	port      = 2222
	password  = "quay-test"
	repo      = "https://github.com/LuwuDynamics/xgoduck_runtime_arduino"
	folder    = "xgoduck_runtime_arduino"
	appDir    = "/home/arduino/ArduinoApps/" + folder
)

type pw struct{ asked int }

func (p *pw) Secret(context.Context, string, string) (string, bool, error) {
	p.asked++
	return password, true, nil
}
func (p *pw) TrustHostKey(context.Context, string, string, string) (bool, error) {
	return true, nil
}

func sh(t *testing.T, cmd string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "bash", "-c", cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("board: %s: %v\n%s", cmd, err, out)
	}
	return strings.TrimSpace(string(out))
}

func shOK(cmd string) bool {
	return exec.Command("docker", "exec", container, "bash", "-c", cmd).Run() == nil
}

func TestMain(m *testing.M) {
	dir, _ := filepath.Abs("../fakeboard")
	_ = exec.Command("docker", "rm", "-f", container).Run()
	if out, err := exec.Command("docker", "build", "-q", "-t", "quay-fakeboard", dir).CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(1)
	}
	// SYS_ADMIN lets the fake hostnamectl rename the container, like a board renamed by App Lab.
	if out, err := exec.Command("docker", "run", "-d", "--name", container, "--hostname", "duck", "--cap-add", "SYS_ADMIN",
		"-p", "127.0.0.1:2222:22", "quay-fakeboard").CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(1)
	}
	time.Sleep(1500 * time.Millisecond)
	code := m.Run()
	if os.Getenv("KEEP") == "" {
		_ = exec.Command("docker", "rm", "-f", container).Run()
	}
	os.Exit(code)
}

func collect(t *testing.T) (core.Sink, func() []core.Event) {
	var evs []core.Event
	return func(e core.Event) {
			evs = append(evs, e)
			switch e.Type {
			case "step":
				if e.State != "run" {
					t.Logf("  [%s] %s: %s", e.State, e.Step, e.Text)
				}
			case "warn":
				t.Logf("  ! %s", e.Text)
			case "log":
				t.Logf("    | %s", e.Text)
			}
		}, func() []core.Event {
			return evs
		}
}

func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	home := t.TempDir()
	prompt := &pw{}
	target := board.Target{Kind: board.KindSSH, Host: "127.0.0.1", Port: port, User: "arduino"}
	s, err := core.Connect(ctx, target, prompt, filepath.Join(home, "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sink, _ := collect(t)
	backups := filepath.Join(home, "backups")
	cache := filepath.Join(home, "repos")

	t.Run("status on a fresh board", func(t *testing.T) {
		st, err := s.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !st.AppCLI || len(st.Apps) != 0 || st.User != "arduino" {
			t.Fatalf("unexpected status: %+v", st)
		}
	})

	t.Run("rename the board without a password", func(t *testing.T) {
		before := prompt.asked
		if err := s.Rename(ctx, sink, "goose"); err != nil {
			t.Fatal(err)
		}
		if got := sh(t, "hostname"); got != "goose" {
			t.Fatalf("hostname %q", got)
		}
		st, _ := s.Status(ctx)
		if st.Hostname != "goose" {
			t.Fatalf("status hostname %q", st.Hostname)
		}
		if err := s.Rename(ctx, sink, "Bad_Name"); err == nil {
			t.Fatal("an invalid name was accepted")
		}
		if err := s.Rename(ctx, sink, "duck"); err != nil {
			t.Fatal(err)
		}
		if prompt.asked != before {
			t.Fatalf("renaming asked for a password (%d times); the image allows it without one", prompt.asked-before)
		}
		if !strings.Contains(sh(t, "cat /var/log/fake-systemctl.log"), "start avahi-daemon") {
			t.Fatal("avahi-daemon was not restarted to announce the new name")
		}
	})

	t.Run("deploy from GitHub", func(t *testing.T) {
		b, err := core.Fetch(ctx, sink, cache, core.FetchOptions{Repo: "LuwuDynamics/xgoduck_runtime_arduino"})
		if err != nil {
			t.Skipf("GitHub unreachable: %v", err)
		}
		res, err := s.Deploy(ctx, sink, b, core.DeployOptions{Start: true, Backup: true, BackupDir: backups})
		if err != nil {
			t.Fatal(err)
		}
		if res.Folder != folder || res.Profile == nil || res.Profile.ID != "xgoduck" {
			t.Fatalf("folder/profile: %+v", res)
		}
		if !res.Rebuilt || !res.NeedsSetup {
			t.Fatalf("expected a rebuild after patching and a setup hint: %+v", res)
		}
		if got := sh(t, "cat "+appDir+"/.cache/sketch/built-with"); !strings.Contains(got, "4096") {
			t.Fatalf("sketch was not rebuilt against the patched library: %q", got)
		}
		if !strings.Contains(sh(t, "cat "+appDir+"/.quay.json"), b.Commit) {
			t.Fatal(".quay.json does not record the commit")
		}
		st, _ := s.Status(ctx)
		a, err := st.App(folder)
		if err != nil || a.Status != "running" || a.Profile == nil || a.SystemReady == nil || *a.SystemReady {
			t.Fatalf("app state after deploy: %+v %v", a, err)
		}
		if a.WebURL != "http://127.0.0.1:9527/" || len(a.Ports) != 1 || a.Ports[0] != 9527 {
			t.Fatalf("console: url=%q ports=%v", a.WebURL, a.Ports)
		}
	})

	t.Run("prepare (sudo with password)", func(t *testing.T) {
		before := prompt.asked
		if err := s.Prepare(ctx, sink, folder, false, false); err != nil {
			t.Fatal(err)
		}
		if prompt.asked != before {
			t.Logf("password asked %d time(s) (SSH login password reused for sudo)", prompt.asked-before)
		}
		if !shOK("grep -q 2000000 /etc/systemd/system/arduino-router.service.d/90-xgoduck-baud.conf") {
			t.Fatal("router drop-in missing")
		}
		st, _ := s.Status(ctx)
		a, _ := st.App(folder)
		if a.SystemReady == nil || !*a.SystemReady || a.Status != "running" {
			t.Fatalf("after prepare: ready=%v status=%s", a.SystemReady, a.Status)
		}
	})

	t.Run("deploy applies a missing system setup before starting", func(t *testing.T) {
		b, err := core.Fetch(ctx, sink, cache, core.FetchOptions{Repo: repo})
		if err != nil {
			t.Skipf("GitHub unreachable: %v", err)
		}
		steps := func(evs []core.Event) []string {
			var l []string
			for _, e := range evs {
				if e.Type == "step" && e.State == "ok" {
					l = append(l, e.Step)
				}
			}
			return l
		}
		// Done already: the install dialog's checkbox must not stop and restart the app for nothing.
		sk, evs := collect(t)
		if _, err := s.Deploy(ctx, sk, b, core.DeployOptions{Start: true, Setup: true}); err != nil {
			t.Fatal(err)
		}
		if l := strings.Join(steps(evs()), " "); strings.Contains(l, "system") {
			t.Fatalf("setup ran although it was done: %s", l)
		}
		if err := s.Prepare(ctx, sink, folder, true, false); err != nil {
			t.Fatal(err)
		}
		sk, evs = collect(t)
		res, err := s.Deploy(ctx, sk, b, core.DeployOptions{Start: true, Setup: true})
		if err != nil {
			t.Fatal(err)
		}
		l := strings.Join(steps(evs()), " ")
		if res.NeedsSetup || !strings.Contains(l, "system start") {
			t.Fatalf("expected the setup right before the start: needsSetup=%v steps=%s", res.NeedsSetup, l)
		}
		st, _ := s.Status(ctx)
		a, _ := st.App(folder)
		if a.SystemReady == nil || !*a.SystemReady || a.Status != "running" {
			t.Fatalf("after deploy with setup: ready=%v status=%s", a.SystemReady, a.Status)
		}
	})

	t.Run("policies: install, refuse, roll back, keep across deploys, restore", func(t *testing.T) {
		walk := appDir + "/python/xgoduck_walk.onnx"
		boardSum := func(p string) string { return strings.Fields(sh(t, "sha256sum "+p))[0] }
		stock := boardSum(walk)
		ps, err := s.Policies(ctx, folder)
		if err != nil || len(ps.Slots) != 5 || ps.Slots[0].ID != "walk" || ps.Slots[0].Active != nil || ps.Slots[0].SHA256 != stock {
			t.Fatalf("policies: %+v %v", ps, err)
		}
		raw, err := exec.Command("docker", "exec", container, "cat", walk).Output()
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		model := func(name string, meta ...string) string { // the stock model with metadata appended
			b := append([]byte{}, raw...)
			for i := 0; i+1 < len(meta); i += 2 {
				b = append(b, metaField(meta[i], meta[i+1])...)
			}
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}
		v2 := model("v2.onnx", "quay_test", "v2")
		if err := s.InstallPolicy(ctx, sink, folder, core.InstallPolicyOptions{Slot: "walk", File: v2, Meta: core.InstalledPolicy{Name: "Walk v2", Author: "test"}}); err != nil {
			t.Fatal(err)
		}
		v2sum := boardSum(walk)
		if v2sum == stock || boardSum(appDir+"/data/quay-policies/stock/xgoduck_walk.onnx") != stock {
			t.Fatal("the model was not swapped, or the stock one was not kept")
		}
		st, _ := s.Status(ctx)
		a, _ := st.App(folder)
		if a.Policies["walk"].Name != "Walk v2" || a.Status != "running" {
			t.Fatalf("after install: %+v %s", a.Policies, a.Status)
		}

		err = s.InstallPolicy(ctx, sink, folder, core.InstallPolicyOptions{Slot: "walk", File: model("swapped.onnx", "joint_names", "right_hip_yaw,left_hip_yaw")})
		var pe *core.PolicyError
		if !errors.As(err, &pe) || !strings.Contains(err.Error(), "joint_names") || boardSum(walk) != v2sum {
			t.Fatalf("a model for another joint order must be refused: %v", err)
		}

		err = s.InstallPolicy(ctx, sink, folder, core.InstallPolicyOptions{Slot: "walk", File: model("broken.onnx", "quay_test", "QUAY_FAKE_BROKEN"), Meta: core.InstalledPolicy{Name: "Broken"}})
		if err == nil || !strings.Contains(err.Error(), "Walk v2 is back") {
			t.Fatalf("a model the app cannot start with must be rolled back: %v", err)
		}
		st, _ = s.Status(ctx)
		a, _ = st.App(folder)
		if boardSum(walk) != v2sum || a.Policies["walk"].Name != "Walk v2" || a.Status != "running" {
			t.Fatalf("after rollback: %s %+v %s", boardSum(walk), a.Policies, a.Status)
		}

		b, err := core.Fetch(ctx, sink, cache, core.FetchOptions{Repo: repo})
		if err != nil {
			t.Skipf("GitHub unreachable: %v", err)
		}
		if _, err := s.Deploy(ctx, sink, b, core.DeployOptions{Start: true}); err != nil {
			t.Fatal(err)
		}
		if boardSum(walk) != v2sum || boardSum(appDir+"/data/quay-policies/stock/xgoduck_walk.onnx") != stock {
			t.Fatal("a deploy must put the installed policy back over the new stock model")
		}

		if err := s.RestorePolicies(ctx, sink, folder, nil, false); err != nil {
			t.Fatal(err)
		}
		st, _ = s.Status(ctx)
		a, _ = st.App(folder)
		if boardSum(walk) != stock || len(a.Policies) != 0 || a.Status != "running" || shOK("ls "+appDir+"/data/quay-policies/models/*.onnx") {
			t.Fatalf("after restore: %s %+v %s", boardSum(walk), a.Policies, a.Status)
		}
	})

	var backup string
	t.Run("backup data", func(t *testing.T) {
		sh(t, `mkdir -p `+appDir+`/data && printf '%s' '{"zero_pos":[1,2,3]}' > `+appDir+`/data/zero_pos.json && printf '{}' > `+appDir+`/data/gamepad-bindings.json && chown -R arduino `+appDir+`/data`)
		p, err := s.Backup(ctx, sink, folder, backups)
		if err != nil || p == "" {
			t.Fatalf("backup: %q %v", p, err)
		}
		meta, err := core.ReadBackupMeta(p)
		if err != nil || meta.Folder != folder {
			t.Fatalf("meta: %+v %v", meta, err)
		}
		backup = p
	})

	t.Run("safety probe blocks stop while torque is on", func(t *testing.T) {
		sh(t, "touch /tmp/fake-enabled")
		err := s.Stop(ctx, sink, folder, false)
		sh(t, "rm -f /tmp/fake-enabled")
		var unsafe *core.UnsafeError
		if !errors.As(err, &unsafe) || !strings.Contains(err.Error(), "torque") {
			t.Fatalf("expected the safety refusal, got %v", err)
		}
		// Calibration holds servos without "enabled": the profile's extra check covers it.
		sh(t, "echo calibrate > /tmp/fake-mode")
		err = s.Stop(ctx, sink, folder, false)
		sh(t, "rm -f /tmp/fake-mode")
		if !errors.As(err, &unsafe) || !strings.Contains(err.Error(), "Calibration") {
			t.Fatalf("expected the calibration refusal, got %v", err)
		}
	})

	t.Run("update to an older commit keeps data", func(t *testing.T) {
		res, err := s.Update(ctx, sink, cache, folder, "9dc3678", core.DeployOptions{Backup: true, BackupDir: backups})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Commit, "9dc3678") || res.Previous == res.Commit {
			t.Fatalf("commit: %+v", res)
		}
		if !shOK("test -f " + appDir + "/data/zero_pos.json") {
			t.Fatal("data/ lost on update")
		}
		if res.Backup == "" {
			t.Fatal("expected an automatic backup")
		}
	})

	t.Run("restore", func(t *testing.T) {
		sh(t, "echo '{\"broken\": true}' > "+appDir+"/data/zero_pos.json")
		if err := s.Restore(ctx, sink, folder, backup, false); err != nil {
			t.Fatal(err)
		}
		if got := sh(t, "cat "+appDir+"/data/zero_pos.json"); !strings.Contains(got, "zero_pos") {
			t.Fatalf("not restored: %s", got)
		}
		if !shOK("ls -d " + appDir + "/data.before-restore-*") {
			t.Fatal("no restore point kept")
		}
	})

	t.Run("mirror on and off", func(t *testing.T) {
		sources := "/etc/apt/sources.list.d/debian.sources"
		orig := sh(t, "sha256sum "+sources)
		ustc, _ := mirror.ByID("ustc")
		if err := s.Mirror(ctx, sink, core.MirrorOptions{PyPI: core.DefaultMirror, Debian: core.DefaultDebianMirror,
			PyPIFallback: ustc.PyPI, DebianFallback: []string{ustc.Debian, "https://mirrors.aliyun.com/debian"}}); err != nil {
			t.Fatal(err)
		}
		if pip := sh(t, "cat /etc/pip.conf"); !strings.Contains(pip, "index-url = https://mirrors.tuna") || !strings.Contains(pip, "extra-index-url = "+ustc.PyPI) {
			t.Fatalf("pip.conf:\n%s", pip)
		}
		if !strings.Contains(sh(t, "cat /etc/uv/uv.toml"), "tuna") {
			t.Fatal("uv mirror not set")
		}
		src := sh(t, "cat "+sources)
		if !strings.Contains(src, "mirror+file:/etc/apt/quay/debian.list") || !strings.Contains(src, "mirror+file:/etc/apt/quay/debian-security.list") || strings.Contains(src, "deb.debian.org") {
			t.Fatalf("apt is not on the mirror lists:\n%s", src)
		}
		// apt tries the mirrors in this order and moves on by itself when one fails.
		want := "https://mirrors.tuna.tsinghua.edu.cn/debian\tpriority:1\nhttps://mirrors.ustc.edu.cn/debian\tpriority:2\n" +
			"https://mirrors.aliyun.com/debian\tpriority:3\nhttp://deb.debian.org/debian\tpriority:4"
		if got := sh(t, "cat /etc/apt/quay/debian.list"); got != want {
			t.Fatalf("debian.list:\n%s\nwant:\n%s", got, want)
		}
		if got := sh(t, "head -1 /etc/apt/quay/debian-security.list"); got != "https://mirrors.tuna.tsinghua.edu.cn/debian-security\tpriority:1" {
			t.Fatalf("debian-security.list: %q", got)
		}
		if shOK("ls /etc/apt/sources.list.d/ | grep -v '\\.sources$\\|\\.list$' | grep -q .") {
			t.Fatal("backups left in sources.list.d, where apt warns about them")
		}
		st, _ := s.Status(ctx)
		if !st.Mirror.Managed || !strings.Contains(st.Mirror.Pip, "tuna") || !strings.Contains(st.Mirror.Debian, "tuna") || len(st.Mirror.DebianFallback) != 3 {
			t.Fatalf("status mirror: %+v", st.Mirror)
		}
		// Switching mirrors rewrites the lists from scratch.
		if err := s.Mirror(ctx, sink, core.MirrorOptions{Debian: ustc.Debian}); err != nil {
			t.Fatal(err)
		}
		if got := sh(t, "cut -f1 /etc/apt/quay/debian.list | tr '\\n' ' '"); got != ustc.Debian+" http://deb.debian.org/debian" {
			t.Fatalf("second mirror: %q", got)
		}
		if err := s.Mirror(ctx, sink, core.MirrorOptions{}); err != nil {
			t.Fatal(err)
		}
		if shOK("test -e /etc/pip.conf") || shOK("ls /etc/pip.conf.quay-* 2>/dev/null | grep -q .") || shOK("test -e /var/lib/quay/apt-orig") || shOK("test -e /etc/apt/quay") {
			t.Fatal("mirror off did not restore the original state")
		}
		if got := sh(t, "sha256sum "+sources); got != orig {
			t.Fatal("the Debian sources are not byte-identical after mirror off")
		}
	})

	t.Run("mirror speed test from the board", func(t *testing.T) {
		// Three local "mirrors" on the board: a good one, one without security updates, a dead one.
		sh(t, `. /etc/os-release; a=$(dpkg --print-architecture); for m in good nosec; do
			mkdir -p /tmp/mirrors/$m/debian/dists/$VERSION_CODENAME/main/binary-$a /tmp/mirrors/$m/pypi/pip
			echo Release > /tmp/mirrors/$m/debian/dists/$VERSION_CODENAME/Release
			head -c 3000000 /dev/urandom > /tmp/mirrors/$m/debian/dists/$VERSION_CODENAME/main/binary-$a/Packages.xz
			echo '<a href="pip-26.2.2-py3-none-any.whl">pip</a>' > /tmp/mirrors/$m/pypi/pip/index.html
		done
		mkdir -p /tmp/mirrors/good/debian-security/dists/$VERSION_CODENAME-security
		echo Release > /tmp/mirrors/good/debian-security/dists/$VERSION_CODENAME-security/Release
		(cd /tmp/mirrors && nohup python3 -m http.server 8088 --bind 127.0.0.1 >/dev/null 2>&1 &) ; sleep 1`)
		base := "http://127.0.0.1:8088/"
		list := []mirror.Mirror{
			{ID: "dead", Debian: "http://127.0.0.1:9/debian", PyPI: "http://127.0.0.1:9/pypi"},
			{ID: "nosec", Debian: base + "nosec/debian", PyPI: base + "nosec/pypi"},
			{ID: "good", Debian: base + "good/debian", PyPI: base + "good/pypi"},
		}
		var streamed []string
		rs, err := s.TestMirrors(ctx, func(e core.Event) {
			if r, ok := e.Data.(mirror.Result); ok && e.Type == "info" {
				streamed = append(streamed, r.ID)
			}
			sink(e)
		}, list)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(streamed, ",") != "dead,nosec,good" {
			t.Fatalf("results should stream in test order: %v", streamed)
		}
		if len(rs) != 3 || rs[0].ID != "good" || !rs[0].Works() || rs[0].Debian.Speed <= 0 || !rs[0].PyPI.OK {
			t.Fatalf("best: %+v", rs)
		}
		by := map[string]mirror.Result{}
		for _, r := range rs {
			by[r.ID] = r
		}
		if r := by["nosec"]; !r.Debian.OK || r.Debian.Security || r.Works() {
			t.Fatalf("no security updates: %+v %+v", r.Debian, r.PyPI)
		}
		if r := by["dead"]; r.Debian.OK || r.PyPI.OK || r.Debian.Error == "" {
			t.Fatalf("dead mirror: %+v %+v", r.Debian, r.PyPI)
		}
		sh(t, "pkill -f 'http[.]server 8088' || true")
	})

	t.Run("upload the SSH public key", func(t *testing.T) {
		key := filepath.Join(t.TempDir(), "id_ed25519")
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "quay-e2e", "-f", key).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
		_, line, err := board.LocalPublicKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if added, err := s.UploadKey(ctx, sink, line); err != nil || !added {
			t.Fatalf("first upload: %v %v", added, err)
		}
		if added, err := s.UploadKey(ctx, sink, line); err != nil || added {
			t.Fatalf("second upload should find it there: %v %v", added, err)
		}
		if n := sh(t, "grep -c quay-e2e /home/arduino/.ssh/authorized_keys"); n != "1" {
			t.Fatalf("key lines: %s", n)
		}
		if perms := sh(t, "stat -c %a /home/arduino/.ssh /home/arduino/.ssh/authorized_keys | tr '\\n' ' '"); perms != "700 600" {
			t.Fatalf("permissions: %q", perms)
		}
		st, _ := s.Status(ctx)
		found := false
		for _, k := range st.AuthorizedKeys {
			found = found || k == board.KeyBlob(line)
		}
		if !found {
			t.Fatal("status does not list the uploaded key")
		}
	})

	t.Run("relay downloads through this computer", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "a .deb from the computer") }))
		defer srv.Close()
		if err := s.Relay(ctx, sink, true); err != nil {
			t.Fatal(err)
		}
		if got := sh(t, "/usr/local/lib/quay/apt-proxy"); got != "DIRECT" {
			t.Fatalf("hook without Quay connected: %q", got)
		}
		if err := s.StartRelay(&relay.Proxy{AllowAnyPort: true}); err != nil {
			t.Fatal(err)
		}
		if got := sh(t, "/usr/local/lib/quay/apt-proxy"); got != "http://127.0.0.1:18765" {
			t.Fatalf("hook with Quay connected: %q", got)
		}
		// The test server listens on this computer's loopback: only the relay can reach it.
		fetch := `python3 -c "import urllib.request as u; print(u.build_opener(u.ProxyHandler({'http': 'http://127.0.0.1:18765'})).open('` + srv.URL + `', timeout=10).read().decode())"`
		if got := sh(t, fetch); got != "a .deb from the computer" {
			t.Fatalf("download through the relay: %q", got)
		}
		st, _ := s.Status(ctx)
		if !st.Relay.Installed || !st.Relay.Active {
			t.Fatalf("status relay: %+v", st.Relay)
		}
		s.StopRelay()
		if got := sh(t, "/usr/local/lib/quay/apt-proxy"); got != "DIRECT" {
			t.Fatalf("hook after stopping: %q", got)
		}
		if err := s.Relay(ctx, sink, false); err != nil {
			t.Fatal(err)
		}
		if shOK("test -e /etc/apt/apt.conf.d/80quay-relay || test -e /usr/local/lib/quay/apt-proxy") {
			t.Fatal("relay off left the hook behind")
		}
	})

	t.Run("system update without a password", func(t *testing.T) {
		before := prompt.asked
		u, err := s.CheckUpgrades(ctx, sink, true)
		if err != nil {
			t.Fatal(err)
		}
		if u.Total.Count != 8 || u.Arduino.Count != 3 || u.Desktop.Count != 3 || len(u.Sources) != 2 {
			t.Fatalf("pending: %+v %+v %+v %+v", u.Total, u.Arduino, u.Desktop, u.Sources)
		}
		// Downloading first fills the board's apt cache, with progress, and installs nothing.
		sk, evs := collect(t)
		res, err := s.Upgrade(ctx, sk, core.UpgradeOptions{SkipDesktop: true, DownloadOnly: true})
		if err != nil || !res.Downloaded {
			t.Fatalf("download only: %+v %v", res, err)
		}
		var last core.Progress
		for _, e := range evs() {
			if e.Type == "progress" && e.Step == "download" {
				last = e.Data.(core.Progress)
			}
		}
		if last.Percent != 100 || last.Unit != "bytes" || last.Total == 0 {
			t.Fatalf("download progress: %+v", last)
		}
		if u2, _ := s.CheckUpgrades(ctx, sink, false); u2.Total.Count != 8 || u2.Total.Size >= u.Total.Size {
			t.Fatalf("after downloading, nothing should be installed and less left to fetch: %+v", u2.Total)
		}
		// Leaving out the desktop runs Quay's own apt commands…
		sk, evs = collect(t)
		res, err = s.Upgrade(ctx, sk, core.UpgradeOptions{SkipDesktop: true})
		if err != nil {
			t.Fatal(err)
		}
		last = core.Progress{}
		for _, e := range evs() {
			if e.Type == "progress" && e.Step == "upgrade" {
				last = e.Data.(core.Progress)
			}
		}
		if last.Percent != 100 || last.Unit != "packages" {
			t.Fatalf("install progress: %+v", last)
		}
		if res.Remaining != 3 || !strings.Contains(res.AppCLIVersion, "0.14.0") {
			t.Fatalf("after skipping the desktop: %+v", res)
		}
		if sh(t, "cat /tmp/quay-upgrade/rc") != "0" {
			t.Fatal("the detached job did not record success")
		}
		// …everything else goes through the board's own `arduino-app-cli system update`.
		if _, err := s.Upgrade(ctx, sink, core.UpgradeOptions{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sh(t, "cat /tmp/fake-appcli/calls.log"), "system update --yes") {
			t.Fatal("arduino-app-cli system update was not used")
		}
		u, err = s.CheckUpgrades(ctx, sink, false)
		if err != nil || u.Total.Count != 0 {
			t.Fatalf("still pending: %+v %v", u, err)
		}
		if prompt.asked != before {
			t.Fatalf("the update asked for a password (%d times); the image allows it without one", prompt.asked-before)
		}
	})

	t.Run("autostart", func(t *testing.T) {
		if err := s.Autostart(ctx, sink, folder); err != nil {
			t.Fatal(err)
		}
		st, _ := s.Status(ctx)
		if a, _ := st.App(folder); !a.Default {
			t.Fatal("not default")
		}
	})

	t.Run("export and logs", func(t *testing.T) {
		f := filepath.Join(home, "export.zip")
		out, _ := os.Create(f)
		err := s.Export(ctx, sink, folder, out)
		out.Close()
		if fi, _ := os.Stat(f); err != nil || fi.Size() < 1000 {
			t.Fatalf("export: %v", err)
		}
		var lines int
		if err := s.Logs(ctx, func(e core.Event) {
			if e.Type == "log" {
				lines++
			}
		}, folder, false, 50); err != nil || lines == 0 {
			t.Fatalf("logs: %d lines, %v", lines, err)
		}
	})

	t.Run("erase with backup and system revert", func(t *testing.T) {
		p, err := s.Erase(ctx, sink, folder, core.EraseOptions{Backup: true, BackupDir: backups, RevertSystem: true})
		if err != nil {
			t.Fatal(err)
		}
		if p == "" {
			t.Fatal("no backup before erase")
		}
		if shOK("test -e " + appDir) {
			t.Fatal("folder still there")
		}
		if shOK("test -e /var/lib/arduino-app-cli/default.app") {
			t.Fatal("boot default not cleared")
		}
		if shOK("test -e /etc/systemd/system/arduino-router.service.d/90-xgoduck-baud.conf") {
			t.Fatal("system setup not reverted")
		}
		if got := sh(t, "cat /home/arduino/.arduino15/internal/Arduino_RouterBridge_*/Arduino_RouterBridge/src/bridge.h"); !strings.Contains(got, " 500") {
			t.Fatalf("library patch not undone: %s", got)
		}
		st, _ := s.Status(ctx)
		if len(st.Apps) != 0 {
			t.Fatalf("apps left: %+v", st.Apps)
		}
	})
	t.Logf("fake app-cli calls:\n%s", sh(t, "cat /tmp/fake-appcli/calls.log"))
}

// TestCLI drives the quay binary (CLI build) with key auth against the same fake board,
// deploying a generic app (no quay.yaml) from a local folder.
func TestCLI(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "quay")
	if out, err := exec.Command("go", "build", "-tags", "nogui", "-o", bin, "../..").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	key := filepath.Join(home, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	sh(t, "mkdir -p /home/arduino/.ssh && echo '"+strings.TrimSpace(string(pub))+"' >> /home/arduino/.ssh/authorized_keys && chown -R arduino /home/arduino/.ssh && chmod 700 /home/arduino/.ssh && chmod 600 /home/arduino/.ssh/authorized_keys")
	scan, err := exec.Command("ssh-keyscan", "-p", "2222", "127.0.0.1").Output()
	if err != nil || len(scan) == 0 {
		t.Fatalf("ssh-keyscan: %v", err)
	}
	cfg := filepath.Join(home, "cfg")
	os.MkdirAll(cfg, 0o700)
	os.WriteFile(filepath.Join(cfg, "known_hosts"), scan, 0o600)

	app := filepath.Join(home, "hello_unoq")
	os.MkdirAll(filepath.Join(app, "python"), 0o755)
	os.WriteFile(filepath.Join(app, "app.yaml"), []byte("name: Hello UNO Q\ndescription: test app\nicon: 👋\n"), 0o644)
	os.WriteFile(filepath.Join(app, "python", "main.py"), []byte("print('hi')\n"), 0o644)

	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), "QUAY_HOME="+cfg, "SSH_AUTH_SOCK=", "NO_COLOR=1")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("quay %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	// Boards are named after themselves: the fake board's hostname is "duck".
	run("board", "add", "arduino@127.0.0.1:2222", "--key", key, "--default")
	if out := run("board", "ls"); !strings.Contains(out, "* duck") {
		t.Fatalf("board ls: %s", out)
	}
	run("rename", "goose")
	if out := run("board", "ls"); !strings.Contains(out, "* goose") || !strings.Contains(out, "127.0.0.1:2222") {
		t.Fatalf("after rename: %s", out)
	}
	run("rename", "duck")
	if out := run("upgrade", "--check", "--no-refresh"); !strings.Contains(out, "up to date") && !strings.Contains(out, "updates") {
		t.Fatalf("upgrade --check: %s", out)
	}
	out := run("deploy", app)
	if !strings.Contains(out, "done") || !strings.Contains(out, "hello_unoq") {
		t.Fatalf("deploy: %s", out)
	}
	out = run("status")
	if !strings.Contains(out, "Hello UNO Q") || !strings.Contains(out, "running") {
		t.Fatalf("status: %s", out)
	}
	run("stop", "hello_unoq")
	run("autostart", "hello_unoq")
	out = run("--json", "status")
	if !strings.Contains(out, `"default": true`) {
		t.Fatalf("json status: %s", out)
	}
	out = run("erase", "hello_unoq", "--yes")
	if !strings.Contains(out, "erased hello_unoq") || shOK("test -e /home/arduino/ArduinoApps/hello_unoq") {
		t.Fatalf("erase: %s", out)
	}
	t.Logf("CLI ok")
}

// metaField encodes an ONNX metadata_props entry (ModelProto field 14). Appending it to a model is
// valid protobuf; a later entry for the same key wins.
func metaField(k, v string) []byte {
	str := func(num byte, s string) []byte { return append(append([]byte{num<<3 | 2}, varint(len(s))...), s...) }
	entry := append(str(1, k), str(2, v)...)
	return append(append([]byte{14<<3 | 2}, varint(len(entry))...), entry...)
}

func varint(n int) []byte {
	var b []byte
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	return append(b, byte(n))
}
