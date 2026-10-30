package selfupdate

import (
	"net/url"
	"os"
	"path/filepath"

	"github.com/caoxg/quay/internal/config"
	"github.com/caoxg/quay/internal/relay"
)

// New is an Updater that downloads through this computer's proxy, like the download relay: the
// relay's upstream setting, else HTTPS_PROXY, else the system proxy settings.
func New() *Updater {
	return &Updater{Proxy: func() (*url.URL, error) {
		up := ""
		if cfg, err := config.Load(); err == nil {
			up = cfg.RelayUpstream
		}
		return relay.Upstream(up)
	}}
}

// CheckFile is where the last update check is kept.
func CheckFile() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "Quay", "update.json")
	}
	return filepath.Join(config.Dir(), "update.json")
}
