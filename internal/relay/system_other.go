//go:build !darwin && !windows

package relay

// systemProxy: on Linux the environment (HTTPS_PROXY) is the convention, read by Upstream.
func systemProxy() string { return "" }
