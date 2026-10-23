package mirror

import "testing"

func TestRankAndPick(t *testing.T) {
	rs := []Result{
		{ID: "tuna", Debian: &Probe{OK: true, MS: 240, Speed: 3 << 20, Security: true}, PyPI: &Probe{OK: true, MS: 270}},
		{ID: "aliyun", Debian: &Probe{OK: true, MS: 60, Speed: 11 << 20, Security: true}, PyPI: &Probe{OK: true, MS: 35}},
		{ID: "ustc", Debian: &Probe{OK: false, Error: "timed out"}, PyPI: &Probe{OK: true, MS: 130}},
		{ID: "nju", Debian: &Probe{OK: true, MS: 80, Speed: 20 << 20, Security: false}, PyPI: &Probe{OK: true, MS: 50}},
		{ID: "official", Debian: &Probe{OK: true, MS: 430, Speed: 15 << 20, Security: true}, PyPI: &Probe{OK: true, MS: 570}},
		{ID: "huawei", Debian: &Probe{OK: true, MS: 70, Speed: 8 << 20, Security: true}, PyPI: &Probe{OK: false, Error: "HTTP 502"}},
	}
	ranked := Rank(rs)
	want := []string{"official", "aliyun", "tuna"} // working ones by speed; nju lacks security updates
	for i, id := range want {
		if ranked[i].ID != id {
			t.Fatalf("rank %d: got %s, want %s (%v)", i, ranked[i].ID, id, ranked)
		}
	}
	for _, r := range ranked[3:] {
		if r.Works() {
			t.Errorf("%s should rank among the failed", r.ID)
		}
	}
	picked := Pick(rs, 2)
	if len(picked) != 2 || picked[0].ID != "aliyun" || picked[1].ID != "tuna" {
		t.Fatalf("pick: %+v", picked) // the official servers are never picked as a mirror
	}
}

func TestMatch(t *testing.T) {
	if m, ok := Match("https://mirrors.aliyun.com/debian/"); !ok || m.ID != "aliyun" {
		t.Errorf("aliyun: %v %v", m, ok)
	}
	if m, ok := Match("https://repo.huaweicloud.com/repository/pypi/simple"); !ok || m.ID != "huawei" {
		t.Errorf("huawei pypi: %v %v", m, ok)
	}
	if m, ok := Match("http://deb.debian.org/debian"); !ok || m.ID != "official" {
		t.Errorf("official: %v %v", m, ok)
	}
	if _, ok := Match("https://example.org/debian"); ok {
		t.Error("unknown host matched")
	}
	if _, ok := ByID("TUNA"); !ok {
		t.Error("ByID is case-insensitive")
	}
	if Speed(11.5*(1<<20)) != "11.5 MB/s" || Speed(300*1024) != "300 KB/s" {
		t.Errorf("speed: %s %s", Speed(11.5*(1<<20)), Speed(300*1024))
	}
}
