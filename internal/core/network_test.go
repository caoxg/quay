package core

import "testing"

func TestValidSSIDAndPSK(t *testing.T) {
	for _, ok := range []string{"home", "Wei's iPhone", "小鸭子", "a-b_c.d 1"} {
		if err := ValidSSID(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " lead", "trail ", "semi;colon", `back\slash`, "tab\there", "123456789012345678901234567890123"} {
		if ValidSSID(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"12345678", "p@ss word;\\", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789ABCDEF"} {
		if err := ValidPSK(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"short", "密码密码密码密码", "0123456789012345678901234567890123456789012345678901234567890123x"} {
		if ValidPSK(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
