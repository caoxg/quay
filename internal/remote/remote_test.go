package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The UNO Q has /etc/apt/preferences.d but no /etc/apt/preferences: find's complaint about the
// missing path once failed the whole script (pipefail), and the update's second round went unasked.
func TestPrefsChangedWithoutPreferencesFile(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	run, etc := filepath.Join(dir, "quay-upgrade"), filepath.Join(dir, "apt")
	for _, d := range []string{run, filepath.Join(etc, "preferences.d")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	started := filepath.Join(run, "started")
	if err := os.WriteFile(started, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	hourAgo := time.Now().Add(-time.Hour)
	if err := os.Chtimes(started, hourAgo, hourAgo); err != nil {
		t.Fatal(err)
	}
	pins := filepath.Join(etc, "preferences.d", "arduino-pins.pref")
	if err := os.WriteFile(pins, []byte("Package: src:alsa-ucm-conf:any\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReplacer("/tmp/quay-upgrade", run, "/etc/apt", etc).Replace(PrefsChanged)
	var stderr strings.Builder
	cmd := exec.Command("bash", "-c", Script(body, nil))
	cmd.Stderr = &stderr // the prelude's getent is missing on macOS
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v\n%s%s", err, out, stderr.String())
	}
	if !strings.HasPrefix(string(out), pins+" ") {
		t.Fatalf("the changed preference file is not listed: %q", out)
	}
}

// The update scripts that carry other scripts (a heredoc job, an embedded python check) still parse.
func TestUpgradeScriptsParse(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	for name, body := range map[string]string{"UpgradeJob": UpgradeJob, "UpgradeFollow": UpgradeFollow, "PrefsChanged": PrefsChanged, "Indexes": Indexes} {
		c := exec.Command("bash", "-n")
		c.Stdin = strings.NewReader(Script(body, map[string]string{"CMD": "true", "ROUND": "test", "FOLDER": "x"}))
		if out, err := c.CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
}
