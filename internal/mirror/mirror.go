// Package mirror lists package mirrors for boards in mainland China and ranks them by a speed test
// run on the board (core.Session.TestMirrors).
package mirror

import (
	"fmt"
	"sort"
	"strings"
)

// Mirror serves PyPI and Debian; its Debian security updates are at Debian + "-security".
type Mirror struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	NameZH string `json:"nameZh"`
	PyPI   string `json:"pypi"`
	Debian string `json:"debian"`
}

// China are well-run mirrors in mainland China that carry PyPI and Debian (arm64, with security
// updates). All answered for Debian 13 and PyPI when this list was last checked (2026-10).
var China = []Mirror{
	{"tuna", "Tsinghua TUNA", "清华 TUNA", "https://mirrors.tuna.tsinghua.edu.cn/pypi/web/simple", "https://mirrors.tuna.tsinghua.edu.cn/debian"},
	{"ustc", "USTC", "中科大 USTC", "https://mirrors.ustc.edu.cn/pypi/simple", "https://mirrors.ustc.edu.cn/debian"},
	{"aliyun", "Alibaba Cloud", "阿里云", "https://mirrors.aliyun.com/pypi/simple", "https://mirrors.aliyun.com/debian"},
	{"tencent", "Tencent Cloud", "腾讯云", "https://mirrors.cloud.tencent.com/pypi/simple", "https://mirrors.cloud.tencent.com/debian"},
	{"huawei", "Huawei Cloud", "华为云", "https://repo.huaweicloud.com/repository/pypi/simple", "https://repo.huaweicloud.com/debian"},
	{"bfsu", "BFSU", "北外 BFSU", "https://mirrors.bfsu.edu.cn/pypi/web/simple", "https://mirrors.bfsu.edu.cn/debian"},
	{"nju", "Nanjing University", "南京大学", "https://mirror.nju.edu.cn/pypi/web/simple", "https://mirror.nju.edu.cn/debian"},
	{"sjtu", "SJTU", "上海交大", "https://mirror.sjtu.edu.cn/pypi/web/simple", "https://mirror.sjtu.edu.cn/debian"},
}

// Official is where the packages come from without a mirror: tested alongside for comparison, and
// apt's last resort when every mirror fails.
var Official = Mirror{ID: "official", Name: "Official servers", NameZH: "官方源", PyPI: "https://pypi.org/simple", Debian: "http://deb.debian.org/debian"}

// Default is used when nothing has been tested.
var Default = China[0]

// ByID finds a mirror in China (or Official).
func ByID(id string) (Mirror, bool) {
	for _, m := range append(append([]Mirror{}, China...), Official) {
		if strings.EqualFold(m.ID, id) {
			return m, true
		}
	}
	return Mirror{}, false
}

// Match names the mirror a PyPI or Debian URL belongs to.
func Match(u string) (Mirror, bool) {
	h := host(u)
	if h == "" {
		return Mirror{}, false
	}
	for _, m := range append(append([]Mirror{}, China...), Official) {
		if host(m.PyPI) == h || host(m.Debian) == h {
			return m, true
		}
	}
	return Mirror{}, false
}

func host(u string) string {
	_, rest, ok := strings.Cut(u, "://")
	if !ok {
		return ""
	}
	h, _, _ := strings.Cut(rest, "/")
	return strings.ToLower(h)
}

// Probe is how one index answered.
type Probe struct {
	OK       bool    `json:"ok"`
	MS       int     `json:"ms,omitempty"`       // time to the first byte
	Speed    float64 `json:"speed,omitempty"`    // download speed in bytes/s (Debian only)
	Security bool    `json:"security,omitempty"` // Debian security updates answered too
	Error    string  `json:"error,omitempty"`
}

// Result is one mirror's test.
type Result struct {
	ID     string `json:"id"`
	Debian *Probe `json:"debian,omitempty"`
	PyPI   *Probe `json:"pypi,omitempty"`
}

// Works reports whether the mirror served what was tested.
func (r Result) Works() bool {
	return (r.Debian == nil || (r.Debian.OK && r.Debian.Security)) && (r.PyPI == nil || r.PyPI.OK)
}

// Summary is a one-line description for logs.
func (r Result) Summary() string {
	var parts []string
	if d := r.Debian; d != nil {
		switch {
		case !d.OK:
			parts = append(parts, "Debian failed: "+d.Error)
		case !d.Security:
			parts = append(parts, fmt.Sprintf("Debian %s (%d ms), no security updates", Speed(d.Speed), d.MS))
		default:
			parts = append(parts, fmt.Sprintf("Debian %s (%d ms)", Speed(d.Speed), d.MS))
		}
	}
	if p := r.PyPI; p != nil {
		if p.OK {
			parts = append(parts, fmt.Sprintf("PyPI %d ms", p.MS))
		} else {
			parts = append(parts, "PyPI failed: "+p.Error)
		}
	}
	return r.ID + ": " + strings.Join(parts, ", ")
}

// Speed formats bytes/s.
func Speed(bps float64) string {
	switch {
	case bps >= 1<<20:
		return fmt.Sprintf("%.1f MB/s", bps/(1<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.0f KB/s", bps/(1<<10))
	}
	return fmt.Sprintf("%.0f B/s", bps)
}

// Rank orders results best first: working mirrors by Debian download speed (or PyPI latency when
// only PyPI was tested), then the ones that failed.
func Rank(rs []Result) []Result {
	out := append([]Result{}, rs...)
	score := func(r Result) (int, float64) {
		if !r.Works() {
			return 0, 0
		}
		if r.Debian != nil {
			return 2, r.Debian.Speed
		}
		return 1, 1e9 / float64(r.PyPI.MS+1)
	}
	sort.SliceStable(out, func(i, j int) bool {
		gi, si := score(out[i])
		gj, sj := score(out[j])
		if gi != gj {
			return gi > gj
		}
		return si > sj
	})
	return out
}

// Pick returns up to n working mirrors from China, best first (the official servers are not a
// mirror: apt falls back to them by itself).
func Pick(rs []Result, n int) []Mirror {
	var out []Mirror
	for _, r := range Rank(rs) {
		if !r.Works() || r.ID == Official.ID {
			continue
		}
		if m, ok := ByID(r.ID); ok {
			out = append(out, m)
		}
		if len(out) == n {
			break
		}
	}
	return out
}
