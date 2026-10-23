package legal

import (
	"os/exec"
	"strings"
	"testing"
)

// The embedded notices must name every module the binary is built from; a new dependency without
// `make licenses` fails here.
func TestThirdPartyCoversDependencies(t *testing.T) {
	for _, tags := range []string{"production", "nogui"} {
		out, err := exec.Command("go", "list", "-deps", "-tags", tags,
			"-f", "{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}", "github.com/caoxg/quay").Output()
		if err != nil {
			t.Fatalf("go list: %v", err)
		}
		for _, m := range strings.Fields(strings.ReplaceAll(string(out), " ", "@")) {
			path, version, _ := strings.Cut(m, "@")
			if !strings.Contains(ThirdParty, "\n"+path+" "+version+" — ") {
				t.Errorf("%s %s is missing from THIRD_PARTY_LICENSES.txt; run `make licenses`", path, version)
			}
		}
	}
}
