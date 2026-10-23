package relay

import "os/exec"

// systemProxy reads the HTTPS (else HTTP) proxy from the macOS network settings, which is where
// proxy apps such as Clash put it in "system proxy" mode.
func systemProxy() string {
	out, err := exec.Command("scutil", "--proxy").Output()
	if err != nil {
		return ""
	}
	return parseScutil(string(out))
}
