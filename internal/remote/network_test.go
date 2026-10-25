package remote

import (
	"os/exec"
	"strings"
	"testing"
)

func TestHotspotScriptsParse(t *testing.T) {
	if strings.Contains(hotspotService, "\nQUAY_HOTSPOT_EOF\n") || strings.Contains(hotspotUnit, "\nQUAY_UNIT_EOF\n") {
		t.Fatal("an embedded file contains its heredoc delimiter")
	}
	if !strings.Contains(Away, "class Service:") || !strings.Contains(Away, "ExecStart=/usr/local/lib/quay/quay-hotspot run") {
		t.Fatal("Away does not carry the service and its unit")
	}
	if _, err := exec.LookPath("bash"); err == nil {
		for name, body := range map[string]string{"WifiAdd": WifiAdd, "WifiPhone": WifiPhone, "WifiRemove": WifiRemove,
			"HotspotSettings": HotspotSettings, "Away": Away, "HotspotRequest": HotspotRequest} {
			c := exec.Command("bash", "-n")
			c.Stdin = strings.NewReader(RootScript(body, map[string]string{"SECRETS": "/tmp/x", "UUID": "u", "WHAT": "open", "MODE": "board", "PHONE": "yes"}))
			if out, err := c.CombinedOutput(); err != nil {
				t.Errorf("%s: %v\n%s", name, err, out)
			}
		}
	}
	if _, err := exec.LookPath("python3"); err == nil {
		for name, src := range map[string]string{"Network": Network, "WifiScan": WifiScan, "Status": Status, "quay-hotspot": hotspotService} {
			c := exec.Command("python3", "-c", "import ast, sys; ast.parse(sys.stdin.read())")
			c.Stdin = strings.NewReader(src)
			if out, err := c.CombinedOutput(); err != nil {
				t.Errorf("%s: %v\n%s", name, err, out)
			}
		}
	}
}

// TestHotspotService runs the service's own tests (hotspot/test_quay_hotspot.py).
func TestHotspotService(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	c := exec.Command("python3", "-m", "unittest", "test_quay_hotspot")
	c.Dir = "hotspot"
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
