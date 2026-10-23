package relay

import "strings"

// parseScutil reads `scutil --proxy` output (macOS).
func parseScutil(out string) string {
	kv := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	for _, p := range []string{"HTTPS", "HTTP"} {
		if kv[p+"Enable"] == "1" && kv[p+"Proxy"] != "" {
			port := kv[p+"Port"]
			if port == "" {
				port = "80"
			}
			return "http://" + kv[p+"Proxy"] + ":" + port
		}
	}
	return ""
}

// parseWindowsProxy handles "host:port" and "http=host:port;https=host:port".
func parseWindowsProxy(s string) string {
	if !strings.Contains(s, "=") {
		return strings.TrimSpace(s)
	}
	m := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			m[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if m["https"] != "" {
		return m["https"]
	}
	return m["http"]
}
