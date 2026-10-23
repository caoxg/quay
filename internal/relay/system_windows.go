package relay

import "golang.org/x/sys/windows/registry"

// systemProxy reads the proxy from the Windows Internet settings ("Use a proxy server").
func systemProxy() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	if on, _, err := k.GetIntegerValue("ProxyEnable"); err != nil || on == 0 {
		return ""
	}
	s, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return ""
	}
	return parseWindowsProxy(s)
}
